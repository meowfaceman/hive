package remotemachineset

import (
	"context"
	"fmt"
	"math/rand"
	"strings"

	"github.com/blang/semver"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machineapi "github.com/openshift/cluster-api/pkg/apis/machine/v1beta1"
	installgcp "github.com/openshift/installer/pkg/asset/machines/gcp"
	installertypes "github.com/openshift/installer/pkg/types"
	installertypesgcp "github.com/openshift/installer/pkg/types/gcp"

	hivev1 "github.com/openshift/hive/pkg/apis/hive/v1"
	"github.com/openshift/hive/pkg/constants"
	controllerutils "github.com/openshift/hive/pkg/controller/utils"
	"github.com/openshift/hive/pkg/gcpclient"
)

const (
	// Omit m, the installer used this for the master machines. w is also removed as this is implicitly used
	// by the installer for the original worker pool.
	validLeaseChars = "abcdefghijklnopqrstuvxyz0123456789"
)

var (
	versionsSupportingFullNames = semver.MustParseRange(">=4.4.7")
)

// GCPActuator encapsulates the pieces necessary to be able to generate
// a list of MachineSets to sync to the remote cluster.
type GCPActuator struct {
	client    client.Client
	gcpClient gcpclient.Client
	logger    log.FieldLogger
	scheme    *runtime.Scheme
	projectID string
	// expectations is a reference to the reconciler's TTLCache of machinepoolnamelease creates each machinepool
	// expects to see.
	expectations   controllerutils.ExpectationsInterface
	leasesRequired bool
}

var _ Actuator = &GCPActuator{}

// NewGCPActuator is the constructor for building a GCPActuator
func NewGCPActuator(
	client client.Client,
	gcpCreds *corev1.Secret,
	clusterVersion string,
	remoteMachineSets []machineapi.MachineSet,
	scheme *runtime.Scheme,
	expectations controllerutils.ExpectationsInterface,
	logger log.FieldLogger,
) (*GCPActuator, error) {
	gcpClient, err := gcpclient.NewClientFromSecret(gcpCreds)
	if err != nil {
		logger.WithError(err).Warn("failed to create GCP client with creds in clusterDeployment's secret")
		return nil, err
	}

	projectID, err := gcpclient.ProjectIDFromSecret(gcpCreds)
	if err != nil {
		logger.WithError(err).Error("error getting project ID from GCP credentials secret")
		return nil, err
	}

	actuator := &GCPActuator{
		gcpClient:      gcpClient,
		client:         client,
		logger:         logger,
		scheme:         scheme,
		expectations:   expectations,
		projectID:      projectID,
		leasesRequired: requireLeases(clusterVersion, remoteMachineSets, logger),
	}
	return actuator, nil
}

// GenerateMachineSets satisfies the Actuator interface and will take a clusterDeployment and return a list of MachineSets
// to sync to the remote cluster.
func (a *GCPActuator) GenerateMachineSets(cd *hivev1.ClusterDeployment, pool *hivev1.MachinePool, logger log.FieldLogger) ([]*machineapi.MachineSet, bool, error) {
	if cd.Spec.ClusterMetadata == nil {
		return nil, false, errors.New("ClusterDeployment does not have cluster metadata")
	}
	if cd.Spec.Platform.GCP == nil {
		return nil, false, errors.New("ClusterDeployment is not for GCP")
	}
	if pool.Spec.Platform.GCP == nil {
		return nil, false, errors.New("MachinePool is not for GCP")
	}
	clusterVersion, err := getClusterVersion(cd)
	if err != nil {
		return nil, false, fmt.Errorf("Unable to get cluster version: %v", err)
	}

	leases := &hivev1.MachinePoolNameLeaseList{}
	err = a.client.List(context.TODO(), leases, client.InNamespace(pool.Namespace),
		client.MatchingLabels(map[string]string{
			constants.ClusterDeploymentNameLabel: cd.Name,
		}))
	if err != nil {
		logger.WithError(err).Log(controllerutils.LogLevel(err), "error fetching machinepoolleases")
		return nil, false, err
	}

	poolName := pool.Spec.Name

	// If leases are required by the cluster version or existing "w" worker machinesets in the cluster or are already
	// being used as indicated by the existence of MachinePoolLeases, then use leases for determining the machine pool
	// name.
	useLeases := true
	switch {
	case a.leasesRequired:
		logger.Debug("using leases since they are required by the cluster")
	case len(leases.Items) > 0:
		logger.Debug("using leases since there are existing MachinePoolNameLeases")
	default:
		logger.Debug("not using leases")
		useLeases = false
	}
	if useLeases {
		leaseChar, proceed, err := a.obtainLease(pool, cd, leases)
		if err != nil {
			logger.WithError(err).Log(controllerutils.LogLevel(err), "error obtaining pool name lease")
			return nil, false, err
		}
		if !proceed {
			return nil, false, nil
		}
		poolName = leaseChar
	}

	ic := &installertypes.InstallConfig{
		Platform: installertypes.Platform{
			GCP: &installertypesgcp.Platform{
				Region:    cd.Spec.Platform.GCP.Region,
				ProjectID: a.projectID,
			},
		},
	}

	computePool := baseMachinePool(pool)
	computePool.Name = poolName
	computePool.Platform.GCP = &installertypesgcp.MachinePool{
		Zones:        pool.Spec.Platform.GCP.Zones,
		InstanceType: pool.Spec.Platform.GCP.InstanceType,
		OSDisk: installertypesgcp.OSDisk{
			DiskType:   "pd-ssd",
			DiskSizeGB: 128,
		},
	}

	// get image ID for the generated machine sets
	imageID, err := a.getImageID(cd, logger)
	if err != nil {
		return nil, false, errors.Wrap(err, "failed to find image ID for the machine sets")
	}

	if len(computePool.Platform.GCP.Zones) == 0 {
		zones, err := a.getZones(cd.Spec.Platform.GCP.Region)
		if err != nil {
			return nil, false, errors.Wrap(err, "compute pool not providing list of zones and failed to fetch list of zones")
		}
		if len(zones) == 0 {
			return nil, false, fmt.Errorf("zero zones returned for region %s", cd.Spec.Platform.GCP.Region)
		}
		computePool.Platform.GCP.Zones = zones
	}

	workerUserDataSecret, err := workerUserData(clusterVersion)

	if err != nil {
		return nil, false, fmt.Errorf("error determining worker user data secret: %v", err)
	}

	// Assuming all machine pools are workers at this time.
	installerMachineSets, err := installgcp.MachineSets(cd.Spec.ClusterMetadata.InfraID, ic, computePool, imageID, workerRole, workerUserDataSecret)
	return installerMachineSets, err == nil, errors.Wrap(err, "failed to generate machinesets")
}

func (a *GCPActuator) getZones(region string) ([]string, error) {
	zones := []string{}

	// Filter to regions matching '.*<region>.*' (where the zone is actually UP)
	zoneFilter := fmt.Sprintf("(region eq '.*%s.*') (status eq UP)", region)

	pageToken := ""

	for {
		zoneList, err := a.gcpClient.ListComputeZones(gcpclient.ListComputeZonesOptions{
			Filter:    zoneFilter,
			PageToken: pageToken,
		})
		if err != nil {
			return zones, err
		}

		for _, zone := range zoneList.Items {
			zones = append(zones, zone.Name)
		}

		if zoneList.NextPageToken == "" {
			break
		}
		pageToken = zoneList.NextPageToken
	}

	return zones, nil
}

func (a *GCPActuator) getImageID(cd *hivev1.ClusterDeployment, logger log.FieldLogger) (string, error) {
	infra := cd.Spec.ClusterMetadata.InfraID

	// find names of the form '<infra>-.*'
	filter := fmt.Sprintf("name eq \"%s-.*\"", infra)
	result, err := a.gcpClient.ListComputeImages(gcpclient.ListComputeImagesOptions{Filter: filter})
	if err != nil {
		logger.WithError(err).Warnf("failed to find a GCP image starting with name: %s", infra)
		return "", err
	}
	switch len(result.Items) {
	case 0:
		msg := fmt.Sprintf("found 0 results searching for GCP image starting with name: %s", infra)
		logger.Warnf(msg)
		return "", errors.New(msg)
	case 1:
		logger.Debugf("using image with name %s for machine sets", result.Items[0].Name)
		return result.Items[0].Name, nil
	default:
		msg := fmt.Sprintf("unexpected number of results when looking for GCP image with name starting with %s", infra)
		logger.Warnf(msg)
		return "", errors.New(msg)
	}
}

// obtainLease uses the Hive MachinePoolNameLease resource to obtain a unique, single character
// for use in the name of the machine pool. We are severely restricted on name lengths on GCP
// and effectively have one character of flexibility with the naming convention originating in
// the installer.
func (a *GCPActuator) obtainLease(pool *hivev1.MachinePool, cd *hivev1.ClusterDeployment, leases *hivev1.MachinePoolNameLeaseList) (leaseChar string, proceed bool, leaseErr error) {
	for _, l := range leases.Items {
		if l.Labels[constants.MachinePoolNameLabel] == pool.Name {
			a.logger.Debugf("machine pool already has lease: %s", l.Name)
			// Ensure the lease name is in the format we expect, we know everything up to
			// the last character.
			leaseChar := l.Name[len(l.Name)-1:]
			expectedLeaseName := fmt.Sprintf("%s-%s", cd.Spec.ClusterMetadata.InfraID, leaseChar)
			if expectedLeaseName != l.Name {
				return "", false, fmt.Errorf("lease %s did not match expected lease name format (%s[CHAR])", l.Name, expectedLeaseName)
			}
			return leaseChar, true, nil
		}
	}

	a.logger.Debugf("machine pool does not have a lease yet")

	var leaseRune rune
	// If the pool.Spec.Name == "worker", we want to preserve this MachinePool's "w" character that
	// the installer would have selected so we do not cycle all worker nodes.
	// Despite the separation of pool.Name and pool.Spec.Name, we do know that only one pool will
	// have pool.Spec.Name worker as we validate that the pool must be named
	// [clusterdeploymentname]-[pool.spec.name]
	if pool.Spec.Name == "worker" {
		leaseRune = 'w'
		a.logger.Debug("selecting lease char 'w' for original worker pool")
	} else {
		// Pool does not have a lease yet, lookup all currently available lease chars
		availLeaseChars, err := a.findAvailableLeaseChars(cd, leases)
		if err != nil {
			return "", false, err
		}
		if len(availLeaseChars) == 0 {
			a.logger.Warn("no GCP MachinePoolNameLease characters available, setting condition")
			conds, changed := controllerutils.SetMachinePoolConditionWithChangeCheck(
				pool.Status.Conditions,
				hivev1.NoMachinePoolNameLeasesAvailable,
				corev1.ConditionTrue,
				"OutOfMachinePoolNames",
				"All machine pool names are in use",
				controllerutils.UpdateConditionIfReasonOrMessageChange,
			)
			if changed {
				pool.Status.Conditions = conds
				if err := a.client.Status().Update(context.Background(), pool); err != nil {
					return "", false, err
				}
			}
			// Nothing else we can do, wait for requeue when a lease frees up
			return "", false, nil
		}
		// Ensure the above condition is not set if it shouldn't be.
		conds, changed := controllerutils.SetMachinePoolConditionWithChangeCheck(
			pool.Status.Conditions,
			hivev1.NoMachinePoolNameLeasesAvailable,
			corev1.ConditionFalse,
			"MachinePoolNamesAvailable",
			"Machine pool names available",
			controllerutils.UpdateConditionNever,
		)
		if changed {
			pool.Status.Conditions = conds
			err := a.client.Status().Update(context.Background(), pool)
			if err != nil {
				return "", false, err
			}
		}

		// Choose a random entry in the available chars to limit collisions while processing
		// multiple machine pools at the same time. In this case the subsequent attempts to create
		// the lease will fail due to name collisions and re-reconcile.
		a.logger.Debug("selecting random lease char from available")
		leaseRune = availLeaseChars[rand.Intn(len(availLeaseChars))]
	}
	a.logger.Debugf("selected lease char: %s", string(leaseRune))

	leaseName := fmt.Sprintf("%s-%s", cd.Spec.ClusterMetadata.InfraID, string(leaseRune))
	// Attempt to claim the lease:
	l := &hivev1.MachinePoolNameLease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      leaseName,
			Namespace: pool.Namespace,
			Labels: map[string]string{
				constants.MachinePoolNameLabel:       pool.Name,
				constants.ClusterDeploymentNameLabel: cd.Name,
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "hive.openshift.io/v1",
					Kind:       "MachinePool",
					Name:       pool.Name,
					UID:        pool.UID,
					Controller: pointer.BoolPtr(true),
				},
			},
		},
	}
	a.logger.Debug("adding expectation for lease creation for this pool")
	expectKey := types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}.String()
	a.expectations.ExpectCreations(expectKey, 1)
	if err := a.client.Create(context.TODO(), l); err != nil {
		a.expectations.DeleteExpectations(expectKey)
		return "", false, err
	}
	a.logger.WithField("lease", leaseName).Infof("created lease, waiting until creation is observed")

	return string(leaseRune), false, nil
}

func stringToRuneSet(s string) map[rune]bool {
	chars := make(map[rune]bool)
	for _, char := range s {
		chars[char] = true
	}
	return chars
}

func (a *GCPActuator) findAvailableLeaseChars(cd *hivev1.ClusterDeployment, leases *hivev1.MachinePoolNameLeaseList) ([]rune, error) {
	availChars := stringToRuneSet(validLeaseChars)

	for _, lease := range leases.Items {
		if lease.Labels[constants.ClusterDeploymentNameLabel] != cd.Name {
			// doesn't match this cluster, shouldn't be possible due to filtering in caller, but
			// just in case
			continue
		}

		// Lease name is [infraid]-[x], we need to parse out the 'x'.
		char := lease.Name[len(lease.Name)-1]
		delete(availChars, rune(char))
	}

	keys := make([]rune, len(availChars))
	i := 0
	for k := range availChars {
		keys[i] = k
		i++
	}

	return keys, nil
}

func requireLeases(clusterVersion string, remoteMachineSets []machineapi.MachineSet, logger log.FieldLogger) bool {
	logger = logger.WithField("clusterVersion", clusterVersion)
	if v, err := semver.ParseTolerant(clusterVersion); err == nil {
		if !versionsSupportingFullNames(v) {
			logger.Debug("leases are required since cluster does not support full machine names")
			return true
		}
	}
	poolNames := make(map[string]bool)
	for _, ms := range remoteMachineSets {
		nameParts := strings.Split(ms.Name, "-")
		if len(nameParts) < 3 {
			continue
		}
		poolName := nameParts[len(nameParts)-2]
		poolNames[poolName] = true
	}
	// If there are machinesets with a pool name of "w" and no machinesets with a pool name of "worker", then assume
	// that the "w" pool is the worker pool created by the installer. If the installer-created "w" worker pool still
	// exists, then we must continue to use leases.
	// This will cause problems if a machineset is created on the cluster with a "w" pool name that is not the
	// installer-created worker pool when there are Hive-managed pools that are not using leases. Hive will block
	// through validation MachinePools with a pool name of "w", but the user could still create such machinesets on
	// the cluster manually.
	if poolNames["w"] && !poolNames["worker"] {
		logger.Debug("leases are required since there is a \"w\" machine pool in the cluster that is likely the installer-created worker pool")
		return true
	}
	logger.Debug("leases are not required")
	return false
}
