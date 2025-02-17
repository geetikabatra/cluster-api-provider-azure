/*
Copyright 2020 The Kubernetes Authors.

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

package managedclusters

import (
	"context"
	"fmt"
	"net"

	"github.com/Azure/azure-sdk-for-go/services/containerservice/mgmt/2021-05-01/containerservice"
	"github.com/Azure/go-autorest/autorest/to"
	"github.com/google/go-cmp/cmp"
	"github.com/pkg/errors"
	"k8s.io/klog/v2"

	infrav1alpha4 "sigs.k8s.io/cluster-api-provider-azure/api/v1alpha4"
	"sigs.k8s.io/cluster-api-provider-azure/azure"
	"sigs.k8s.io/cluster-api-provider-azure/util/tele"
)

var (
	defaultUser     string = "azureuser"
	managedIdentity string = "msi"
)

// Spec contains properties to create a managed cluster.
type Spec struct {
	// Name is the name of this AKS Cluster.
	Name string

	// ResourceGroupName is the name of the Azure resource group for this AKS Cluster.
	ResourceGroupName string

	// NodeResourceGroupName is the name of the Azure resource group containing IaaS VMs.
	NodeResourceGroupName string

	// VnetSubnetID is the Azure Resource ID for the subnet which should contain nodes.
	VnetSubnetID string

	// Location is a string matching one of the canonical Azure region names. Examples: "westus2", "eastus".
	Location string

	// Tags is a set of tags to add to this cluster.
	Tags map[string]string

	// Version defines the desired Kubernetes version.
	Version string

	// LoadBalancerSKU for the managed cluster. Possible values include: 'Standard', 'Basic'. Defaults to Standard.
	LoadBalancerSKU string

	// NetworkPlugin used for building Kubernetes network. Possible values include: 'azure', 'kubenet'. Defaults to azure.
	NetworkPlugin string

	// NetworkPolicy used for building Kubernetes network. Possible values include: 'calico', 'azure'. Defaults to azure.
	NetworkPolicy string

	// SSHPublicKey is a string literal containing an ssh public key. Will autogenerate and discard if not provided.
	SSHPublicKey string

	// AgentPools is the list of agent pool specifications in this cluster.
	AgentPools []PoolSpec

	// PodCIDR is the CIDR block for IP addresses distributed to pods
	PodCIDR string

	// ServiceCIDR is the CIDR block for IP addresses distributed to services
	ServiceCIDR string

	// DNSServiceIP is an IP address assigned to the Kubernetes DNS service
	DNSServiceIP *string
}

// PoolSpec contains agent pool specification details.
type PoolSpec struct {
	Name         string
	SKU          string
	Replicas     int32
	OSDiskSizeGB int32
}

// Get fetches a managed cluster from Azure.
func (s *Service) Get(ctx context.Context, spec interface{}) (interface{}, error) {
	ctx, span := tele.Tracer().Start(ctx, "managedclusters.Service.Get")
	defer span.End()

	managedClusterSpec, ok := spec.(*Spec)
	if !ok {
		return nil, errors.New("expected managed cluster specification")
	}
	return s.Client.Get(ctx, managedClusterSpec.ResourceGroupName, managedClusterSpec.Name)
}

// GetCredentials fetches a managed cluster kubeconfig from Azure.
func (s *Service) GetCredentials(ctx context.Context, group, name string) ([]byte, error) {
	ctx, span := tele.Tracer().Start(ctx, "managedclusters.Service.GetCredentials")
	defer span.End()

	return s.Client.GetCredentials(ctx, group, name)
}

// Reconcile idempotently creates or updates a managed cluster, if possible.
func (s *Service) Reconcile(ctx context.Context, spec interface{}) error {
	ctx, span := tele.Tracer().Start(ctx, "managedclusters.Service.Reconcile")
	defer span.End()

	managedClusterSpec, ok := spec.(*Spec)
	if !ok {
		return errors.New("expected managed cluster specification")
	}

	managedCluster := containerservice.ManagedCluster{
		Identity: &containerservice.ManagedClusterIdentity{
			Type: containerservice.ResourceIdentityTypeSystemAssigned,
		},
		Location: &managedClusterSpec.Location,
		Tags:     *to.StringMapPtr(managedClusterSpec.Tags),
		ManagedClusterProperties: &containerservice.ManagedClusterProperties{
			NodeResourceGroup: &managedClusterSpec.NodeResourceGroupName,
			DNSPrefix:         &managedClusterSpec.Name,
			KubernetesVersion: &managedClusterSpec.Version,
			LinuxProfile: &containerservice.LinuxProfile{
				AdminUsername: &defaultUser,
				SSH: &containerservice.SSHConfiguration{
					PublicKeys: &[]containerservice.SSHPublicKey{
						{
							KeyData: &managedClusterSpec.SSHPublicKey,
						},
					},
				},
			},
			ServicePrincipalProfile: &containerservice.ManagedClusterServicePrincipalProfile{
				ClientID: &managedIdentity,
			},
			AgentPoolProfiles: &[]containerservice.ManagedClusterAgentPoolProfile{},
			NetworkProfile: &containerservice.NetworkProfile{
				NetworkPlugin:   containerservice.NetworkPlugin(managedClusterSpec.NetworkPlugin),
				LoadBalancerSku: containerservice.LoadBalancerSku(managedClusterSpec.LoadBalancerSKU),
				NetworkPolicy:   containerservice.NetworkPolicy(managedClusterSpec.NetworkPolicy),
			},
		},
	}

	if managedClusterSpec.PodCIDR != "" {
		managedCluster.NetworkProfile.PodCidr = &managedClusterSpec.PodCIDR
	}

	if managedClusterSpec.ServiceCIDR != "" {
		if managedClusterSpec.DNSServiceIP == nil {
			managedCluster.NetworkProfile.ServiceCidr = &managedClusterSpec.ServiceCIDR
			ip, _, err := net.ParseCIDR(managedClusterSpec.ServiceCIDR)
			if err != nil {
				return fmt.Errorf("failed to parse service cidr: %w", err)
			}
			// HACK: set the last octet of the IP to .10
			// This ensures the dns IP is valid in the service cidr without forcing the user
			// to specify it in both the Capi cluster and the Azure control plane.
			// https://golang.org/src/net/ip.go#L48
			ip[15] = byte(10)
			dnsIP := ip.String()
			managedCluster.NetworkProfile.DNSServiceIP = &dnsIP
		} else {
			managedCluster.NetworkProfile.DNSServiceIP = managedClusterSpec.DNSServiceIP
		}
	}

	for i := range managedClusterSpec.AgentPools {
		pool := managedClusterSpec.AgentPools[i]
		profile := containerservice.ManagedClusterAgentPoolProfile{
			Name:         &pool.Name,
			VMSize:       &pool.SKU,
			OsDiskSizeGB: &pool.OSDiskSizeGB,
			Count:        &pool.Replicas,
			Type:         containerservice.AgentPoolTypeVirtualMachineScaleSets,
			VnetSubnetID: &managedClusterSpec.VnetSubnetID,
			Mode:         containerservice.AgentPoolModeSystem,
		}
		*managedCluster.AgentPoolProfiles = append(*managedCluster.AgentPoolProfiles, profile)
	}

	existingMC, err := s.Client.Get(ctx, managedClusterSpec.ResourceGroupName, managedClusterSpec.Name)
	if err != nil && !azure.ResourceNotFound(err) {
		return errors.Wrap(err, "failed to get existing managed cluster")
	}

	isCreate := azure.ResourceNotFound(err)
	if isCreate {
		err = s.Client.CreateOrUpdate(ctx, managedClusterSpec.ResourceGroupName, managedClusterSpec.Name, managedCluster)
		if err != nil {
			return fmt.Errorf("failed to create managed cluster, %w", err)
		}
	} else {
		ps := *existingMC.ManagedClusterProperties.ProvisioningState
		if ps != string(infrav1alpha4.Canceled) && ps != string(infrav1alpha4.Failed) && ps != string(infrav1alpha4.Succeeded) {
			msg := fmt.Sprintf("Unable to update existing managed cluster in non terminal state. Managed cluster must be in one of the following provisioning states: canceled, failed, or succeeded. Actual state: %s", ps)
			klog.V(2).Infof(msg)
			return errors.New(msg)
		}

		// Normalize properties for the desired (CR spec) and existing managed
		// cluster, so that we check only those fields that were specified in
		// the initial CreateOrUpdate request and that can be modified.
		// Without comparing to normalized properties, we would always get a
		// difference in desired and existing, which would result in sending
		// unnecessary Azure API requests.
		propertiesNormalized := &containerservice.ManagedClusterProperties{
			KubernetesVersion: managedCluster.ManagedClusterProperties.KubernetesVersion,
		}
		existingMCPropertiesNormalized := &containerservice.ManagedClusterProperties{
			KubernetesVersion: existingMC.ManagedClusterProperties.KubernetesVersion,
		}

		diff := cmp.Diff(propertiesNormalized, existingMCPropertiesNormalized)
		if diff != "" {
			klog.V(2).Infof("Update required (+new -old):\n%s", diff)
			err = s.Client.CreateOrUpdate(ctx, managedClusterSpec.ResourceGroupName, managedClusterSpec.Name, managedCluster)
			if err != nil {
				return fmt.Errorf("failed to update managed cluster, %w", err)
			}
		}
	}

	return nil
}

// Delete deletes the virtual network with the provided name.
func (s *Service) Delete(ctx context.Context, spec interface{}) error {
	ctx, span := tele.Tracer().Start(ctx, "managedclusters.Service.Delete")
	defer span.End()

	managedClusterSpec, ok := spec.(*Spec)
	if !ok {
		return errors.New("expected managed cluster specification")
	}

	klog.V(2).Infof("Deleting managed cluster  %s ", managedClusterSpec.Name)
	err := s.Client.Delete(ctx, managedClusterSpec.ResourceGroupName, managedClusterSpec.Name)
	if err != nil {
		if azure.ResourceNotFound(err) {
			// already deleted
			return nil
		}
		return errors.Wrapf(err, "failed to delete managed cluster %s in resource group %s", managedClusterSpec.Name, managedClusterSpec.ResourceGroupName)
	}

	klog.V(2).Infof("successfully deleted managed cluster %s ", managedClusterSpec.Name)
	return nil
}
