// Copyright 2021 The Kubeswitch authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package store

import (
	"sync"

	"github.com/danielfoehrkn/kubeswitch/pkg/store/doks"
	"github.com/digitalocean/doctl/do"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice"
	awseks "github.com/aws/aws-sdk-go-v2/service/eks"
	eks "github.com/aws/aws-sdk-go-v2/service/eks/types"
	gardenclient "github.com/danielfoehrkn/kubeswitch/pkg/store/gardener/copied_gardenctlv2"
	"github.com/danielfoehrkn/kubeswitch/types"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	seedmanagementv1alpha1 "github.com/gardener/gardener/pkg/apis/seedmanagement/v1alpha1"
	vaultapi "github.com/hashicorp/vault/api"
	"github.com/linode/linodego"
	"github.com/ovh/go-ovh/ovh"
	"github.com/rancher/norman/clientbase"
	managementClient "github.com/rancher/rancher/pkg/client/generated/management/v3"
	"github.com/scaleway/scaleway-sdk-go/scw"
	"github.com/sirupsen/logrus"
	gkev1 "google.golang.org/api/container/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// SearchResult is a full kubeconfig path discovered from the kubeconfig store
// given the contained kubeconfig path, the store knows how to retrieve and return the
// actual kubeconfig
type SearchResult struct {
	// KubeconfigPath is the kubeconfig path in the backing store which most of the time encodes enough information to
	// retrieve the kubeconfig associated with it.
	KubeconfigPath string
	// Tags contains the additional metadata that the store wants to associate with a context name.
	// This metadata is later handed over in the getKubeconfigForPath() function when retrieving the kubeconfig bytes for the path and might contain
	// information necessary to retrieve the kubeconfig from the backing store (such a unique ID for the cluster required for the API)
	Tags map[string]string
	// Error is an error which occured when trying to discover kubeconfig paths in the backing store
	Error error
}

type KubeconfigStore interface {
	// GetID returns the unique store ID
	// should be
	// - "<store kind>.default" if the kubeconfigStore.ID is not set
	// - "<store kind>.<id>" if the kubeconfigStore.ID is set
	GetID() string

	// GetKind returns the store kind (e.g., filesystem)
	GetKind() types.StoreKind

	// GetContextPrefix returns the prefix for the kubeconfig context names displayed in the search result
	// includes the path to the kubeconfig in the backing store because some stores compute the prefix based on that
	GetContextPrefix(path string) string

	// VerifyKubeconfigPaths verifies that the configured search paths are valid
	// can also include additional preprocessing
	VerifyKubeconfigPaths() error

	// StartSearch starts the search over the configured search paths
	// and populates the results via the given channel
	StartSearch(channel chan SearchResult)

	// GetKubeconfigForPath returns the byte representation of the kubeconfig
	// the kubeconfig has to fetch the kubeconfig from its backing store (e.g., uses the HTTP API)
	// Optional tags might help identify the cluster in the backing store, but typically such information is already encoded in the kubeconfig path (implementation specific)
	GetKubeconfigForPath(path string, tags map[string]string) ([]byte, error)

	// GetLogger returns the logger of the store
	GetLogger() *logrus.Entry

	// GetStoreConfig returns the store's configuration from the switch config file
	GetStoreConfig() types.KubeconfigStore
}

// Previewer can be optionally implemented by stores to show custom preview content
// before the kubeconfig
type Previewer interface {
	GetSearchPreview(path string, optionalTags map[string]string) (string, error)
}

type FilesystemStore struct {
	Logger                *logrus.Entry
	KubeconfigStore       types.KubeconfigStore
	KubeconfigName        string
	kubeconfigDirectories []string
	kubeconfigFilepaths   []string
}

type VaultStore struct {
	Logger             *logrus.Entry
	KubeconfigStore    types.KubeconfigStore
	Client             *vaultapi.Client
	VaultKeyKubeconfig string
	KubeconfigName     string
	EngineVersion      string
	vaultPaths         []string
}

type GardenerStore struct {
	Logger                    *logrus.Entry
	KubeconfigStore           types.KubeconfigStore
	GardenClient              gardenclient.Client
	Client                    client.Client
	Config                    *types.StoreConfigGardener
	LandscapeIdentity         string
	LandscapeName             string
	StateDirectory            string
	CachePathToShoot          map[string]gardencorev1beta1.Shoot
	PathToShootLock           sync.RWMutex
	CachePathToManagedSeed    map[string]seedmanagementv1alpha1.ManagedSeed
	PathToManagedSeedLock     sync.RWMutex
	CacheCaSecretNameToSecret map[string]corev1.Secret
	CaSecretNameToSecretLock  sync.RWMutex
}

type EKSStore struct {
	Logger          *logrus.Entry
	KubeconfigStore types.KubeconfigStore
	Client          *awseks.Client
	Config          *types.StoreConfigEKS
	// DiscoveredClusters maps the kubeconfig path (az_<resource-group>--<cluster-name>) -> cluster
	// This is a cache for the clusters discovered during the initial search for kubeconfig paths
	// when not using a search index
	DiscoveredClusters map[string]*eks.Cluster
	StateDirectory     string
}

type GKEStore struct {
	Logger          *logrus.Entry
	KubeconfigStore types.KubeconfigStore
	GkeClient       *gkev1.Service
	Config          *types.StoreConfigGKE
	// DiscoveredClusters maps the kubeconfig path (gke--project-name--clusterName) -> cluster
	// This is a cache for the clusters discovered during the initial search for kubeconfig paths
	// when not using a search index
	DiscoveredClusters map[string]*gkev1.Cluster
	// ProjectNameToID contains a mapping projectName -> project ID
	// used to construct the kubeconfig path containing the project name instead of a technical project id
	ProjectNameToID map[string]string
	StateDirectory  string
}

type AzureStore struct {
	Logger *logrus.Entry
	// DiscoveredClustersMutex is a mutex allow many reads, one write mutex to synchronize writes
	// to the DiscoveredClusters map.
	// This can happen when a goroutine still discovers clusters while another goroutine computes the preview for a missing cluster.
	DiscoveredClustersMutex sync.RWMutex
	KubeconfigStore         types.KubeconfigStore
	AksClient               *armcontainerservice.ManagedClustersClient
	Config                  *types.StoreConfigAzure
	// DiscoveredClusters maps the kubeconfig path (az_<resource-group>--<cluster-name>) -> cluster
	// This is a cache for the clusters discovered during the initial search for kubeconfig paths
	// when not using a search index
	DiscoveredClusters map[string]*armcontainerservice.ManagedCluster
	StateDirectory     string
}

type RancherStore struct {
	Logger          *logrus.Entry
	KubeconfigStore types.KubeconfigStore
	ClientOpts      *clientbase.ClientOpts
	Client          *managementClient.Client
}

type OVHStore struct {
	Logger          *logrus.Entry
	KubeconfigStore types.KubeconfigStore
	Client          *ovh.Client
	OVHKubeCache    map[string]OVHKube // map[clusterID]OVHKube
}

type ScalewayStore struct {
	Logger             *logrus.Entry
	KubeconfigStore    types.KubeconfigStore
	Client             *scw.Client
	DiscoveredClusters map[string]ScalewayKube
}

type DigitalOceanStore struct {
	Logger *logrus.Entry
	// DiscoveredClustersMutex is a mutex allow many reads, one write mutex to synchronize writes
	// to the DiscoveredClusters map.
	// This can happen when a goroutine still discovers clusters while another goroutine computes the preview for a missing cluster.
	DiscoveredClustersMutex                   sync.RWMutex
	ContextNameAndClusterNameToClusterIDMutex sync.RWMutex
	KubeconfigStore                           types.KubeconfigStore
	ContextToKubernetesService                map[string]do.KubernetesService
	Config                                    doks.DoctlConfig
}

type AkamaiStore struct {
	Logger          *logrus.Entry
	KubeconfigStore types.KubeconfigStore
	Client          *linodego.Client
	Config          *types.StoreConfigAkamai
}

type CapiStore struct {
	Logger          *logrus.Entry
	KubeconfigStore types.KubeconfigStore
	Client          client.Client
	Config          *types.StoreConfigCapi
}
