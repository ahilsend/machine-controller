package admission

import (
	"encoding/json"
	"fmt"

	admissionv1beta1 "k8s.io/api/admission/v1beta1"
	clusterv1alpha1 "sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
)

func (ad *admissionData) mutateMachineDeployments(ar admissionv1beta1.AdmissionReview) (*admissionv1beta1.AdmissionResponse, error) {

	machineDeployment := clusterv1alpha1.MachineDeployment{}
	if err := json.Unmarshal(ar.Request.Object.Raw, &machineDeployment); err != nil {
		return nil, fmt.Errorf("failed to unmarshal: %v", err)
	}
	machineDeploymentOriginal := machineDeployment.DeepCopy()

	machineDeploymentDefaultingFunction(&machineDeployment)
	if errs := validateMachineDeployment(machineDeployment); len(errs) > 0 {
		return nil, fmt.Errorf("validation failed: %v", errs)
	}

	if err := ad.defaultAndValidateMachineSpec(&machineDeployment.Spec.Template.Spec); err != nil {
		return nil, err
	}

	return createAdmissionResponse(machineDeploymentOriginal, &machineDeployment)
}
