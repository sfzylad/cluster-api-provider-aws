/*
Copyright 2018 The Kubernetes Authors.

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

package machine

// should not need to import the ec2 sdk here
import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/pkg/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/klog"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/apis/awsprovider/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/actuators"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/services/awserrors"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/services/ec2"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/cloud/aws/services/elb"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/deployer"
	"sigs.k8s.io/cluster-api-provider-aws/pkg/tokens"
	clusterv1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
	client "sigs.k8s.io/cluster-api/pkg/client/clientset_generated/clientset/typed/cluster/v1alpha1"
	controllerError "sigs.k8s.io/cluster-api/pkg/controller/error"
)

// Actuator is responsible for performing machine reconciliation.
type Actuator struct {
	*deployer.Deployer

	client client.ClusterV1alpha1Interface
}

// ActuatorParams holds parameter information for Actuator.
type ActuatorParams struct {
	Client client.ClusterV1alpha1Interface
}

// NewActuator returns an actuator.
func NewActuator(params ActuatorParams) *Actuator {
	return &Actuator{
		Deployer: deployer.New(deployer.Params{ScopeGetter: actuators.DefaultScopeGetter}),
		client:   params.Client,
	}
}

func (a *Actuator) getControlPlaneMachines(machineList *clusterv1.MachineList) []*clusterv1.Machine {
	var cpm []*clusterv1.Machine
	for _, m := range machineList.Items {
		if m.Spec.Versions.ControlPlane != "" {
			cpm = append(cpm, m.DeepCopy())
		}
	}
	return cpm
}

// defining equality as name and namespace are equivalent and not checking any other fields.
func machinesEqual(m1 *clusterv1.Machine, m2 *clusterv1.Machine) bool {
	return m1.Name == m2.Name && m1.Namespace == m2.Namespace
}

func (a *Actuator) isNodeJoin(controlPlaneMachines []*clusterv1.Machine, newMachine *clusterv1.Machine, cluster *clusterv1.Cluster) (bool, error) {
	switch newMachine.ObjectMeta.Labels["set"] {
	case "node":
		return true, nil
	case "controlplane":
		contolPlaneExists := false
		for _, cm := range controlPlaneMachines {
			m, err := actuators.NewMachineScope(actuators.MachineScopeParams{
				Machine: cm,
				Cluster: cluster,
				Client:  a.client,
			})
			if err != nil {
				return false, errors.Wrapf(err, "failed to create machine scope for machine %q", cm.Name)
			}

			ec2svc := ec2.NewService(m.Scope)
			contolPlaneExists, err = ec2svc.MachineExists(m)
			if err != nil {
				return false, errors.Wrapf(err, "failed to verify existence of machine %q", m.Name())
			}
			if contolPlaneExists {
				break
			}
		}

		klog.V(2).Infof("Machine %q should join the controlplane: %t", newMachine.Name, contolPlaneExists)
		return contolPlaneExists, nil
	default:
		errMsg := fmt.Sprintf("Unknown value %q for label \"set\" on machine %q, skipping machine creation", newMachine.ObjectMeta.Labels["set"], newMachine.Name)
		klog.Errorf(errMsg)
		err := errors.Errorf(errMsg)
		return false, err
	}
}

// Create creates a machine and is invoked by the machine controller.
func (a *Actuator) Create(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	klog.Infof("Creating machine %v for cluster %v", machine.Name, cluster.Name)

	scope, err := actuators.NewMachineScope(actuators.MachineScopeParams{Machine: machine, Cluster: cluster, Client: a.client})
	if err != nil {
		return errors.Errorf("failed to create scope: %+v", err)
	}

	defer scope.Close()

	ec2svc := ec2.NewService(scope.Scope)

	controlPlaneURL, err := a.GetIP(cluster, nil)
	if err != nil {
		return errors.Errorf("failed to retrieve controlplane url during machine creation: %+v", err)
	}

	clusterMachines, err := scope.MachineClient.List(v1.ListOptions{})
	if err != nil {
		return errors.Wrapf(err, "failed to retrieve machines in cluster %q", cluster.Name)
	}
	controlPlaneMachines := a.getControlPlaneMachines(clusterMachines)
	isNodeJoin, err := a.isNodeJoin(controlPlaneMachines, machine, cluster)
	if err != nil {
		return errors.Wrapf(err, "failed to determine whether machine %q should join cluster %q", machine.Name, cluster.Name)
	}

	var bootstrapToken string
	if isNodeJoin {
		bootstrapToken, err = a.getNodeJoinToken(cluster, controlPlaneURL)
		if err != nil {
			return errors.Wrapf(err, "failed to obtain token for node %q to join cluster %q", machine.Name, cluster.Name)
		}
	}

	kubeConfig, err := a.GetKubeConfig(cluster, nil)
	if err != nil {
		return errors.Wrapf(err, "failed to retrieve kubeconfig while creating machine %q", machine.Name)
	}

	i, err := ec2svc.CreateOrGetMachine(scope, bootstrapToken, kubeConfig)
	if err != nil {
		if awserrors.IsFailedDependency(errors.Cause(err)) {
			klog.Errorf("network not ready to launch instances yet: %+v", err)
			return &controllerError.RequeueAfterError{
				RequeueAfter: time.Minute,
			}
		}

		return errors.Errorf("failed to create or get machine: %+v", err)
	}

	scope.MachineStatus.InstanceID = &i.ID
	scope.MachineStatus.InstanceState = aws.String(string(i.State))

	if machine.Annotations == nil {
		machine.Annotations = map[string]string{}
	}

	machine.Annotations["cluster-api-provider-aws"] = "true"

	if err := a.reconcileLBAttachment(scope, machine, i); err != nil {
		return errors.Errorf("failed to reconcile LB attachment: %+v", err)
	}

	return nil
}

func (a *Actuator) getNodeJoinToken(cluster *clusterv1.Cluster, controlPlaneURL string) (string, error) {
	kubeConfig, err := a.GetKubeConfig(cluster, nil)
	if err != nil {
		return "", errors.Wrapf(err, "failed to retrieve kubeconfig for cluster %q.", cluster.Name)
	}

	clientConfig, err := clientcmd.BuildConfigFromKubeconfigGetter(controlPlaneURL, func() (*clientcmdapi.Config, error) {
		return clientcmd.Load([]byte(kubeConfig))
	})

	if err != nil {
		return "", errors.Wrapf(err, "failed to get client config for cluster at %q", controlPlaneURL)
	}

	coreClient, err := corev1.NewForConfig(clientConfig)
	if err != nil {
		return "", errors.Wrapf(err, "failed to initialize new corev1 client")
	}

	bootstrapToken, err := tokens.NewBootstrap(coreClient, 10*time.Minute)
	if err != nil {
		return "", errors.Wrapf(err, "failed to create new bootstrap token")
	}

	return bootstrapToken, nil
}

func (a *Actuator) reconcileLBAttachment(scope *actuators.MachineScope, m *clusterv1.Machine, i *v1alpha1.Instance) error {
	elbsvc := elb.NewService(scope.Scope)
	if m.ObjectMeta.Labels["set"] == "controlplane" {
		if err := elbsvc.RegisterInstanceWithAPIServerELB(i.ID); err != nil {
			return errors.Wrapf(err, "could not register control plane instance %q with load balancer", i.ID)
		}
	}

	return nil
}

// Delete deletes a machine and is invoked by the Machine Controller
func (a *Actuator) Delete(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	klog.Infof("Deleting machine %v for cluster %v.", machine.Name, cluster.Name)

	scope, err := actuators.NewMachineScope(actuators.MachineScopeParams{Machine: machine, Cluster: cluster, Client: a.client})
	if err != nil {
		return errors.Errorf("failed to create scope: %+v", err)
	}

	defer scope.Close()

	ec2svc := ec2.NewService(scope.Scope)

	instance, err := ec2svc.InstanceIfExists(*scope.MachineStatus.InstanceID)
	if err != nil {
		return errors.Errorf("failed to get instance: %+v", err)
	}

	if instance == nil {
		// The machine hasn't been created yet
		klog.Info("Instance is nil and therefore does not exist")
		return nil
	}

	// Check the instance state. If it's already shutting down or terminated,
	// do nothing. Otherwise attempt to delete it.
	// This decision is based on the ec2-instance-lifecycle graph at
	// https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/ec2-instance-lifecycle.html
	switch instance.State {
	case v1alpha1.InstanceStateShuttingDown, v1alpha1.InstanceStateTerminated:
		klog.Infof("instance %q is shutting down or already terminated", machine.Name)
		return nil
	default:
		if err := ec2svc.TerminateInstance(aws.StringValue(scope.MachineStatus.InstanceID)); err != nil {
			return errors.Errorf("failed to terminate instance: %+v", err)
		}
	}

	klog.Info("shutdown signal was sent. Shutting down machine.")
	return nil
}

// Update updates a machine and is invoked by the Machine Controller.
// If the Update attempts to mutate any immutable state, the method will error
// and no updates will be performed.
func (a *Actuator) Update(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) error {
	klog.Infof("Updating machine %v for cluster %v.", machine.Name, cluster.Name)

	scope, err := actuators.NewMachineScope(actuators.MachineScopeParams{Machine: machine, Cluster: cluster, Client: a.client})
	if err != nil {
		return errors.Errorf("failed to create scope: %+v", err)
	}

	defer scope.Close()

	ec2svc := ec2.NewService(scope.Scope)

	// Get the current instance description from AWS.
	instanceDescription, err := ec2svc.InstanceIfExists(*scope.MachineStatus.InstanceID)
	if err != nil {
		return errors.Errorf("failed to get instance: %+v", err)
	}

	// We can now compare the various AWS state to the state we were passed.
	// We will check immutable state first, in order to fail quickly before
	// moving on to state that we can mutate.
	// TODO: Implement immutable state check.

	// Ensure that the security groups are correct.
	_, err = a.ensureSecurityGroups(
		ec2svc,
		machine,
		*scope.MachineStatus.InstanceID,
		scope.MachineConfig.AdditionalSecurityGroups,
		instanceDescription.SecurityGroupIDs,
	)
	if err != nil {
		return errors.Errorf("failed to apply security groups: %+v", err)
	}

	// Ensure that the tags are correct.
	_, err = a.ensureTags(ec2svc, machine, scope.MachineStatus.InstanceID, scope.MachineConfig.AdditionalTags)
	if err != nil {
		return errors.Errorf("failed to ensure tags: %+v", err)
	}

	return nil
}

// Exists test for the existence of a machine and is invoked by the Machine Controller
func (a *Actuator) Exists(ctx context.Context, cluster *clusterv1.Cluster, machine *clusterv1.Machine) (bool, error) {
	klog.Infof("Checking if machine %v for cluster %v exists", machine.Name, cluster.Name)

	scope, err := actuators.NewMachineScope(actuators.MachineScopeParams{Machine: machine, Cluster: cluster, Client: a.client})
	if err != nil {
		return false, errors.Errorf("failed to create scope: %+v", err)
	}

	defer scope.Close()

	ec2svc := ec2.NewService(scope.Scope)

	// TODO worry about pointers. instance if exists returns *any* instance
	if scope.MachineStatus.InstanceID == nil {
		return false, nil
	}

	instance, err := ec2svc.InstanceIfExists(*scope.MachineStatus.InstanceID)
	if err != nil {
		return false, errors.Errorf("failed to retrieve instance: %+v", err)
	}

	if instance == nil {
		return false, nil
	}

	klog.Infof("Found instance for machine %q: %v", machine.Name, instance)

	switch instance.State {
	case v1alpha1.InstanceStateRunning:
		klog.Infof("Machine %v is running", scope.MachineStatus.InstanceID)
	case v1alpha1.InstanceStatePending:
		klog.Infof("Machine %v is pending", scope.MachineStatus.InstanceID)
	default:
		return false, nil
	}

	if err := a.reconcileLBAttachment(scope, machine, instance); err != nil {
		return true, err
	}

	return true, nil
}
