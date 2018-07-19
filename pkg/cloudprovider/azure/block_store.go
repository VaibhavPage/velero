/*
Copyright 2017 the Heptio Ark contributors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package azure

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/arm/disk"
	"github.com/Azure/azure-sdk-for-go/arm/examples/helpers"
	"github.com/Azure/go-autorest/autorest"
	"github.com/Azure/go-autorest/autorest/azure"
	"github.com/pkg/errors"
	"github.com/satori/uuid"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/heptio/ark/pkg/cloudprovider"
	"github.com/heptio/ark/pkg/util/collections"
)

const (
	azureClientIDKey         = "AZURE_CLIENT_ID"
	azureClientSecretKey     = "AZURE_CLIENT_SECRET"
	azureSubscriptionIDKey   = "AZURE_SUBSCRIPTION_ID"
	azureTenantIDKey         = "AZURE_TENANT_ID"
	azureStorageAccountIDKey = "AZURE_STORAGE_ACCOUNT_ID"
	azureStorageKeyKey       = "AZURE_STORAGE_KEY"
	azureResourceGroupKey    = "AZURE_RESOURCE_GROUP"
	apiTimeoutKey            = "apiTimeout"
	snapshotsResource        = "snapshots"
	disksResource            = "disks"
)

type blockStore struct {
	disks         *disk.DisksClient
	snaps         *disk.SnapshotsClient
	subscription  string
	resourceGroup string
	apiTimeout    time.Duration
}

type snapshotIdentifier struct {
	subscription  string
	resourceGroup string
	name          string
}

func (si *snapshotIdentifier) String() string {
	return getComputeResourceName(si.subscription, si.resourceGroup, snapshotsResource, si.name)
}

func getConfig() map[string]string {
	cfg := map[string]string{
		azureClientIDKey:         "",
		azureClientSecretKey:     "",
		azureSubscriptionIDKey:   "",
		azureTenantIDKey:         "",
		azureStorageAccountIDKey: "",
		azureStorageKeyKey:       "",
		azureResourceGroupKey:    "",
	}

	for key := range cfg {
		cfg[key] = os.Getenv(key)
	}

	return cfg
}

func NewBlockStore() cloudprovider.BlockStore {
	return &blockStore{}
}

func (b *blockStore) Init(config map[string]string) error {
	var (
		apiTimeoutVal = config[apiTimeoutKey]
		apiTimeout    time.Duration
		err           error
	)

	if apiTimeout, err = time.ParseDuration(apiTimeoutVal); err != nil {
		return errors.Wrapf(err, "could not parse %s (expected time.Duration)", apiTimeoutKey)
	}

	if apiTimeout == 0 {
		apiTimeout = 2 * time.Minute
	}

	cfg := getConfig()

	spt, err := helpers.NewServicePrincipalTokenFromCredentials(cfg, azure.PublicCloud.ResourceManagerEndpoint)
	if err != nil {
		return errors.Wrap(err, "error creating new service principal token")
	}

	disksClient := disk.NewDisksClient(cfg[azureSubscriptionIDKey])
	snapsClient := disk.NewSnapshotsClient(cfg[azureSubscriptionIDKey])

	disksClient.PollingDelay = 5 * time.Second
	snapsClient.PollingDelay = 5 * time.Second

	authorizer := autorest.NewBearerAuthorizer(spt)
	disksClient.Authorizer = authorizer
	snapsClient.Authorizer = authorizer

	b.disks = &disksClient
	b.snaps = &snapsClient
	b.subscription = cfg[azureSubscriptionIDKey]
	b.resourceGroup = cfg[azureResourceGroupKey]
	b.apiTimeout = apiTimeout

	return nil
}

func (b *blockStore) CreateVolumeFromSnapshot(snapshotID, volumeType, volumeAZ string, iops *int64) (string, error) {
	snapshotIdentifier, err := b.parseSnapshotName(snapshotID)
	if err != nil {
		return "", err
	}

	// Lookup snapshot info for its Location & Tags so we can apply them to the volume
	snapshotInfo, err := b.snaps.Get(snapshotIdentifier.resourceGroup, snapshotIdentifier.name)
	if err != nil {
		return "", errors.WithStack(err)
	}

	diskName := "restore-" + uuid.NewV4().String()

	disk := disk.Model{
		Name:     &diskName,
		Location: snapshotInfo.Location,
		Properties: &disk.Properties{
			CreationData: &disk.CreationData{
				CreateOption:     disk.Copy,
				SourceResourceID: stringPtr(snapshotIdentifier.String()),
			},
			AccountType: disk.StorageAccountTypes(volumeType),
		},
		Tags: snapshotInfo.Tags,
	}

	ctx, cancel := context.WithTimeout(context.Background(), b.apiTimeout)
	defer cancel()

	_, errChan := b.disks.CreateOrUpdate(b.resourceGroup, *disk.Name, disk, ctx.Done())

	err = <-errChan

	if err != nil {
		return "", errors.WithStack(err)
	}
	return diskName, nil
}

func (b *blockStore) GetVolumeInfo(volumeID, volumeAZ string) (string, *int64, error) {
	res, err := b.disks.Get(b.resourceGroup, volumeID)
	if err != nil {
		return "", nil, errors.WithStack(err)
	}

	return string(res.AccountType), nil, nil
}

func (b *blockStore) IsVolumeReady(volumeID, volumeAZ string) (ready bool, err error) {
	res, err := b.disks.Get(b.resourceGroup, volumeID)
	if err != nil {
		return false, errors.WithStack(err)
	}

	if res.ProvisioningState == nil {
		return false, errors.New("nil ProvisioningState returned from Get call")
	}

	return *res.ProvisioningState == "Succeeded", nil
}

func (b *blockStore) CreateSnapshot(volumeID, volumeAZ string, tags map[string]string) (string, error) {
	// Lookup disk info for its Location
	diskInfo, err := b.disks.Get(b.resourceGroup, volumeID)
	if err != nil {
		return "", errors.WithStack(err)
	}

	fullDiskName := getComputeResourceName(b.subscription, b.resourceGroup, disksResource, volumeID)
	// snapshot names must be <= 80 characters long
	var snapshotName string
	suffix := "-" + uuid.NewV4().String()

	if len(volumeID) <= (80 - len(suffix)) {
		snapshotName = volumeID + suffix
	} else {
		snapshotName = volumeID[0:80-len(suffix)] + suffix
	}

	snap := disk.Snapshot{
		Name: &snapshotName,
		Properties: &disk.Properties{
			CreationData: &disk.CreationData{
				CreateOption:     disk.Copy,
				SourceResourceID: &fullDiskName,
			},
		},
		Tags:     getSnapshotTags(tags, diskInfo.Tags),
		Location: diskInfo.Location,
	}

	ctx, cancel := context.WithTimeout(context.Background(), b.apiTimeout)
	defer cancel()

	_, errChan := b.snaps.CreateOrUpdate(b.resourceGroup, *snap.Name, snap, ctx.Done())
	err = <-errChan

	if err != nil {
		return "", errors.WithStack(err)
	}

	return getComputeResourceName(b.subscription, b.resourceGroup, snapshotsResource, snapshotName), nil
}

func getSnapshotTags(arkTags map[string]string, diskTags *map[string]*string) *map[string]*string {
	if diskTags == nil && len(arkTags) == 0 {
		return nil
	}

	snapshotTags := make(map[string]*string)

	// copy tags from disk to snapshot
	if diskTags != nil {
		for k, v := range *diskTags {
			snapshotTags[k] = stringPtr(*v)
		}
	}

	// merge Ark-assigned tags with the disk's tags (note that we want current
	// Ark-assigned tags to overwrite any older versions of them that may exist
	// due to prior snapshots/restores)
	for k, v := range arkTags {
		// Azure does not allow slashes in tag keys, so replace
		// with dash (inline with what Kubernetes does)
		key := strings.Replace(k, "/", "-", -1)
		snapshotTags[key] = stringPtr(v)
	}

	return &snapshotTags
}

func stringPtr(s string) *string {
	return &s
}

func (b *blockStore) DeleteSnapshot(snapshotID string) error {
	snapshotInfo, err := b.parseSnapshotName(snapshotID)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), b.apiTimeout)
	defer cancel()

	_, errChan := b.snaps.Delete(snapshotInfo.resourceGroup, snapshotInfo.name, ctx.Done())

	err = <-errChan

	// if it's a 404 (not found) error, we don't need to return an error
	// since the snapshot is not there.
	if azureErr, ok := err.(autorest.DetailedError); ok && azureErr.StatusCode == http.StatusNotFound {
		return nil
	}
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func getComputeResourceName(subscription, resourceGroup, resource, name string) string {
	return fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/%s/%s", subscription, resourceGroup, resource, name)
}

var snapshotURIRegexp = regexp.MustCompile(
	`^\/subscriptions\/(?P<subscription>.*)\/resourceGroups\/(?P<resourceGroup>.*)\/providers\/Microsoft.Compute\/snapshots\/(?P<snapshotName>.*)$`)

// parseSnapshotName takes a snapshot name, either fully-qualified or not, and returns
// a snapshot identifier or an error if the name is not in a valid format. If the name
// is not fully-qualified, the subscription and resource group are assumed to be the
// ones that the block store is configured with.
//
// TODO(1.0) remove this function and replace usage with `parseFullSnapshotName` since
// we won't support the legacy snapshot name format for 1.0.
func (b *blockStore) parseSnapshotName(name string) (*snapshotIdentifier, error) {
	switch {
	// legacy format - name only (not fully-qualified)
	case !strings.Contains(name, "/"):
		return &snapshotIdentifier{
			subscription:  b.subscription,
			resourceGroup: b.resourceGroup,
			name:          name,
		}, nil
	// current format - fully qualified
	case snapshotURIRegexp.MatchString(name):
		return parseFullSnapshotName(name)
	// unrecognized format
	default:
		return nil, errors.New("snapshot name is not in a valid format")
	}
}

// parseFullSnapshotName takes a fully-qualified snapshot name and returns
// a snapshot identifier or an error if the snapshot name does not match the
// regexp.
func parseFullSnapshotName(name string) (*snapshotIdentifier, error) {
	submatches := snapshotURIRegexp.FindStringSubmatch(name)
	if len(submatches) != len(snapshotURIRegexp.SubexpNames()) {
		return nil, errors.New("snapshot URI could not be parsed")
	}

	snapshotID := &snapshotIdentifier{}

	// capture names start at index 1 to line up with the corresponding indexes
	// of submatches (see godoc on SubexpNames())
	for i, names := 1, snapshotURIRegexp.SubexpNames(); i < len(names); i++ {
		switch names[i] {
		case "subscription":
			snapshotID.subscription = submatches[i]
		case "resourceGroup":
			snapshotID.resourceGroup = submatches[i]
		case "snapshotName":
			snapshotID.name = submatches[i]
		default:
			return nil, errors.New("unexpected named capture from snapshot URI regex")
		}
	}

	return snapshotID, nil
}

func (b *blockStore) GetVolumeID(pv runtime.Unstructured) (string, error) {
	if !collections.Exists(pv.UnstructuredContent(), "spec.azureDisk") {
		return "", nil
	}

	volumeID, err := collections.GetString(pv.UnstructuredContent(), "spec.azureDisk.diskName")
	if err != nil {
		return "", err
	}

	return volumeID, nil
}

func (b *blockStore) SetVolumeID(pv runtime.Unstructured, volumeID string) (runtime.Unstructured, error) {
	azure, err := collections.GetMap(pv.UnstructuredContent(), "spec.azureDisk")
	if err != nil {
		return nil, err
	}

	azure["diskName"] = volumeID
	azure["diskURI"] = getComputeResourceName(b.subscription, b.resourceGroup, disksResource, volumeID)

	return pv, nil
}
