package admission

import (
	"encoding/json"
	"fmt"

	"github.com/golang/glog"

	clusterv1alpha1conversions "github.com/kubermatic/machine-controller/pkg/apis/cluster/v1alpha1/conversions"
	"github.com/kubermatic/machine-controller/pkg/cloudprovider"
	"github.com/kubermatic/machine-controller/pkg/providerconfig"
	"github.com/kubermatic/machine-controller/pkg/userdata"

	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"

	clusterv1alpha1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
)

func (ad *admissionData) mutateMachines(ar admissionv1beta1.AdmissionReview) (*admissionv1beta1.AdmissionResponse, error) {

	machine := clusterv1alpha1.Machine{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &machine); err != nil {
		return nil, fmt.Errorf("failed to unmarshal: %v", err)
	}
	machineOriginal := machine.DeepCopy()
	glog.V(4).Infof("Defaulting and validating machine %s/%s", machine.Namespace, machine.Name)

	// Mutating .Spec is never allowed
	// Only hidden exception: the machine-controller may set the .Spec.Name to .Metadata.Name
	// because otherwise it can never add the delete finalizer as it internally defaults the Name
	// as well, since on the CREATE request for machines, there is only Metadata.GenerateName set
	// so we can't default it initially
	if ar.Request.Operation == admissionv1beta1.Update {
		oldMachine := clusterv1alpha1.Machine{}
		if err := json.Unmarshal(ar.Request.OldObject.Raw, &oldMachine); err != nil {
			return nil, fmt.Errorf("failed to unmarshal OldObject: %v", err)
		}
		if oldMachine.Spec.Name != machine.Spec.Name && machine.Spec.Name == machine.Name {
			oldMachine.Spec.Name = machine.Spec.Name
		}
		if equal := apiequality.Semantic.DeepEqual(machine.Spec, oldMachine.Spec); !equal {
			return nil, fmt.Errorf("machine.spec is immutable")
		}
	}

	// Add type revision annotation
	if _, ok := machine.Annotations[clusterv1alpha1conversions.TypeRevisionAnnotationName]; !ok {
		if machine.Annotations == nil {
			machine.Annotations = map[string]string{}
		}
		machine.Annotations[clusterv1alpha1conversions.TypeRevisionAnnotationName] = clusterv1alpha1conversions.TypeRevisionCurrentVersion
	}

	// Default name
	if machine.Spec.Name == "" {
		machine.Spec.Name = machine.Name
	}

	// Default and verify .Spec on CREATE only, its expensive and not required to do it on UPDATE
	// as we disallow .Spec changes anyways
	if ar.Request.Operation == admissionv1beta1.Create {
		providerConfig, err := providerconfig.GetConfig(machine.Spec.ProviderConfig)
		if err != nil {
			return nil, fmt.Errorf("failed to read machine.Spec.ProviderConfig: %v", err)
		}
		skg := providerconfig.NewConfigVarResolver(ad.coreClient)
		prov, err := cloudprovider.ForProvider(providerConfig.CloudProvider, skg)
		if err != nil {
			return nil, fmt.Errorf("failed to get cloud provider %q: %v", providerConfig.CloudProvider, err)
		}

		// Verify operating system
		if _, err := userdata.ForOS(providerConfig.OperatingSystem); err != nil {
			return nil, fmt.Errorf("failed to get OS '%s': %v", providerConfig.OperatingSystem, err)
		}

		// Set kubelet version
		if machine.Spec.Versions.Kubelet == "" {
			return nil, fmt.Errorf("Kubelet version must be set")
		}

		defaultedMachineSpec, _, err := prov.AddDefaults(machine.Spec)
		if err != nil {
			return nil, fmt.Errorf("failed to default machineSpec: %v", err)
		}
		machine.Spec = defaultedMachineSpec

		if err := prov.Validate(machine.Spec); err != nil {
			return nil, fmt.Errorf("validation failed: %v", err)
		}
	}

	return createAdmissionResponse(machineOriginal, &machine)
}
