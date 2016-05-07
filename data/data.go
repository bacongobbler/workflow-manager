package data

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/deis/workflow-manager/config"
	"github.com/deis/workflow-manager/types"
	"github.com/satori/go.uuid"
	"k8s.io/kubernetes/pkg/api"
	k8sErrors "k8s.io/kubernetes/pkg/api/errors"
	kcl "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/labels"
)

const (
	deisNamespace      = "deis"
	wfmSecretName      = "deis-workflow-manager"
	clusterIDSecretKey = "cluster-id"
)

var (
	clusterID         string
	availableVersions []types.ComponentVersion
)

// SparseComponentInfo is the JSON compatible struct that holds limited data about a component
type SparseComponentInfo struct {
	Name string `json:"name"`
}

// SparseVersionInfo is the JSON compatible struct that holds limited data about a
// component version
type SparseVersionInfo struct {
	Train string `json:"train"`
}

// SparseComponentAndTrainInfo is the JSON compatible struct that holds a
// SparseComponentInfo and SparseVersionInfo
type SparseComponentAndTrainInfo struct {
	Component SparseComponentInfo `json:"component"`
	Version   SparseVersionInfo   `json:"version"`
}

// SparseComponentAndTrainInfoJSONWrapper is the JSON compatible struct that holds a slice of
// SparseComponentAndTrainInfo structs
type SparseComponentAndTrainInfoJSONWrapper struct {
	Data []SparseComponentAndTrainInfo `json:"data"`
}

// ComponentVersionsJSONWrapper is the JSON compatible struct that holds a slice of
// types.ComponentVersion structs
type ComponentVersionsJSONWrapper struct {
	Data []types.ComponentVersion `json:"data"`
}

// ClusterID is an interface for managing cluster ID data
type ClusterID interface {
	// will have a Get method to retrieve the cluster ID
	Get() (string, error)
}

// ClusterIDFromPersistentStorage fulfills the ClusterID interface
type ClusterIDFromPersistentStorage struct{}

// Get method for ClusterIDFromPersistentStorage
func (c ClusterIDFromPersistentStorage) Get() (string, error) {
	kubeClient, err := kcl.NewInCluster()
	if err != nil {
		log.Printf("Error getting kubernetes client [%s]", err)
		os.Exit(1)
	}
	deisSecrets := kubeClient.Secrets(deisNamespace)
	secret, err := deisSecrets.Get(wfmSecretName)
	if err != nil {
		log.Printf("Error getting secret [%s]", err)
		switch e := err.(type) {
		case *k8sErrors.StatusError:
			// If the error isn't a 404, we don't know how to deal with it
			if e.ErrStatus.Code != 404 {
				return "", err
			}
		default:
			return "", err
		}
	}
	// if we don't have secret data for the cluster ID we assume a new cluster
	// and create a new secret
	if secret.Data[clusterIDSecretKey] == nil {
		newSecret := new(api.Secret)
		newSecret.Name = wfmSecretName
		newSecret.Data = make(map[string][]byte)
		newSecret.Data[clusterIDSecretKey] = []byte(uuid.NewV4().String())
		fromAPI, err := deisSecrets.Create(newSecret)
		if err != nil {
			log.Printf("Error creating new ID [%s]", err)
			return "", err
		}
		secret = fromAPI
	}
	return string(secret.Data[clusterIDSecretKey]), nil
}

// GetID gets the cluster ID
func GetID(id ClusterID) (string, error) {
	// If we haven't yet cached the ID in memory, invoke the passed-in getter
	if clusterID == "" {
		idResponse, err := id.Get()
		if err != nil {
			log.Print(err)
			return "", err
		}
		// And store it in memory
		clusterID = idResponse
	}
	return clusterID, nil
}

// InstalledData is an interface for managing installed cluster metadata
type InstalledData interface {
	// will have a Get method to retrieve installed data
	Get() ([]byte, error)
}

// AvailableComponentVersion is an interface for managing component version data
type AvailableComponentVersion interface {
	// will have a Get method to retrieve available component version data
	Get(component string) (types.Version, error)
}

// InstalledDeisData fulfills the InstalledData interface
type InstalledDeisData struct{}

// Get method for InstalledDeisData
func (g InstalledDeisData) Get() ([]byte, error) {
	rcItems, err := getDeisRCItems()
	var cluster types.Cluster
	for _, rc := range rcItems {
		component := types.ComponentVersion{}
		component.Component.Name = rc.Name
		component.Component.Description = rc.Annotations["chart.helm.sh/description"]
		component.Version.Version = rc.Annotations["chart.helm.sh/version"]
		cluster.Components = append(cluster.Components, component)
	}
	js, err := json.Marshal(cluster)
	if err != nil {
		log.Print(err)
		return []byte{}, err
	}
	return js, nil
}

// LatestReleasedComponent fulfills the AvailableComponentVersion interface
type LatestReleasedComponent struct{}

// Get method for LatestReleasedComponent
func (c LatestReleasedComponent) Get(component string) (types.Version, error) {
	version, err := GetLatestVersion(component)
	if err != nil {
		return types.Version{}, err
	}
	return version, nil
}

// AvailableVersions is an interface for managing available component version data
type AvailableVersions interface {
	// will have a Refresh method to retrieve the version data from the remote authority
	Refresh() ([]types.ComponentVersion, error)
	// will have a Store method to cache the version data in memory
	Store([]types.ComponentVersion)
}

// AvailableVersionsFromAPI fulfills the AvailableVersions interface
type AvailableVersionsFromAPI struct{}

// Refresh method for AvailableVersionsFromAPI
func (a AvailableVersionsFromAPI) Refresh() ([]types.ComponentVersion, error) {
	cluster, err := GetCluster(InstalledDeisData{},
		ClusterIDFromPersistentStorage{},
		LatestReleasedComponent{},
	)
	if err != nil {
		return []types.ComponentVersion{}, err
	}
	reqBody := SparseComponentAndTrainInfoJSONWrapper{}
	for _, component := range cluster.Components {
		sparseComponentAndTrainInfo := SparseComponentAndTrainInfo{}
		sparseComponentAndTrainInfo.Component.Name = component.Component.Name
		sparseComponentAndTrainInfo.Version.Train = component.Version.Train
		reqBody.Data = append(reqBody.Data, sparseComponentAndTrainInfo)
	}
	js, err := json.Marshal(reqBody)
	if err != nil {
		log.Println("error making a JSON representation of cluster data")
		return []types.ComponentVersion{}, err
	}
	var versionsLatestRoute = "/" + config.Spec.APIVersion + "/versions/latest"
	url := config.Spec.VersionsAPIURL + versionsLatestRoute
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(js))
	if err != nil {
		return []types.ComponentVersion{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := getTLSClient().Do(req)
	if err != nil {
		return []types.ComponentVersion{}, err
	}
	defer resp.Body.Close()
	var ret ComponentVersionsJSONWrapper
	json.NewDecoder(resp.Body).Decode(&ret)
	a.Store(ret.Data)
	return ret.Data, nil
}

// Store method for AvailableVersionsFromAPI
func (a AvailableVersionsFromAPI) Store(c []types.ComponentVersion) {
	availableVersions = c
}

// GetAvailableVersions gets available component version data
func GetAvailableVersions(a AvailableVersions) ([]types.ComponentVersion, error) {
	// If we don't have any cached data, get the data from the remote authority
	if len(availableVersions) == 0 {
		log.Println("no available versions data cached")
		componentVersions, err := a.Refresh()
		if err != nil {
			log.Print(err)
			return nil, err
		}
		return componentVersions, nil
	}
	return availableVersions, nil
}

// GetCluster collects all cluster metadata and returns a Cluster
func GetCluster(c InstalledData, i ClusterID, v AvailableComponentVersion) (types.Cluster, error) {
	// Populate cluster object with installed components
	cluster, err := GetInstalled(c)
	if err != nil {
		log.Print(err)
		return types.Cluster{}, err
	}
	err = AddUpdateData(&cluster, v)
	if err != nil {
		log.Print(err)
	}
	// Get the cluster ID
	id, err := GetID(i)
	if err != nil {
		log.Print(err)
		return cluster, err
	}
	// Attach the cluster ID to the components-populated cluster object
	cluster.ID = id
	return cluster, nil
}

// AddUpdateData adds UpdateAvailable field data to cluster components
// Any cluster object modifications are made "in-place"
func AddUpdateData(c *types.Cluster, v AvailableComponentVersion) error {
	// Determine if any components have an available update
	for i, component := range c.Components {
		installed := component.Version.Version
		latest, err := v.Get(component.Component.Name)
		if err != nil {
			return err
		}
		newest := newestVersion(installed, latest.Version)
		if newest != installed {
			c.Components[i].UpdateAvailable = newest
		}
	}
	return nil
}

// GetInstalled collects all installed components and returns a Cluster
func GetInstalled(g InstalledData) (types.Cluster, error) {
	installed, err := g.Get()
	if err != nil {
		log.Print(err)
		return types.Cluster{}, err
	}
	var cluster types.Cluster
	cluster, err = ParseJSONCluster(installed)
	if err != nil {
		log.Print(err)
		return types.Cluster{}, err
	}
	return cluster, nil
}

// GetLatestVersion returns the latest known version of a deis component
func GetLatestVersion(component string) (types.Version, error) {
	var latestVersion types.Version
	latestVersions, err := GetAvailableVersions(AvailableVersionsFromAPI{})
	if err != nil {
		return types.Version{}, err
	}
	for _, componentVersion := range latestVersions {
		if componentVersion.Component.Name == component {
			latestVersion = componentVersion.Version
		}
	}
	if latestVersion.Version == "" {
		return types.Version{}, fmt.Errorf("latest version not available for %s", component)
	}
	return latestVersion, nil
}

// ParseJSONCluster converts a JSON representation of a cluster
// to a Cluster type
func ParseJSONCluster(rawJSON []byte) (types.Cluster, error) {
	var cluster types.Cluster
	err := json.Unmarshal(rawJSON, &cluster)
	if err != nil {
		log.Print(err)
		return types.Cluster{}, err
	}
	return cluster, nil
}

// NewestSemVer returns the newest (largest) semver string
func NewestSemVer(v1 string, v2 string) (string, error) {
	v1Slice := strings.Split(v1, ".")
	v2Slice := strings.Split(v2, ".")
	for i, subVer1 := range v1Slice {
		if v2Slice[i] > subVer1 {
			return v2, nil
		} else if subVer1 > v2Slice[i] {
			return v1, nil
		}
	}
	return v1, nil
}

// getDeisRCItems is a helper function that returns a slice of
// ReplicationController objects in the "deis" namespace
func getDeisRCItems() ([]api.ReplicationController, error) {
	kubeClient, err := kcl.NewInCluster()
	if err != nil {
		log.Printf("Error getting kubernetes client [%s]", err)
		return []api.ReplicationController{}, err
	}
	deis, err := kubeClient.ReplicationControllers(config.Spec.DeisNamespace).List(labels.Everything())
	if err != nil {
		log.Println("unable to get ReplicationControllers() data from kube client")
		log.Print(err)
		return []api.ReplicationController{}, err
	}
	return deis.Items, nil
}

// getTLSClient returns a TLS-enabled http.Client
func getTLSClient() *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: false},
	}
	return &http.Client{Transport: tr}
}

// newestVersion is a temporary static implementation of a real "return newest version" function
func newestVersion(v1 string, v2 string) string {
	return v1
}
