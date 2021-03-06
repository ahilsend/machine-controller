package aws

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"

	"github.com/kubermatic/machine-controller/pkg/cloudprovider/cloud"
	cloudprovidererrors "github.com/kubermatic/machine-controller/pkg/cloudprovider/errors"
	"github.com/kubermatic/machine-controller/pkg/cloudprovider/instance"
	"github.com/kubermatic/machine-controller/pkg/providerconfig"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/iam"
	"github.com/golang/glog"

	common "sigs.k8s.io/cluster-api/pkg/apis/cluster/common"
	"sigs.k8s.io/cluster-api/pkg/apis/cluster/v1alpha1"
)

type provider struct {
	configVarResolver *providerconfig.ConfigVarResolver
}

// New returns a aws provider
func New(configVarResolver *providerconfig.ConfigVarResolver) cloud.Provider {
	return &provider{configVarResolver: configVarResolver}
}

const (
	nameTag       = "Name"
	machineUIDTag = "Machine-UID"

	policyRoute53FullAccess = "arn:aws:iam::aws:policy/AmazonRoute53FullAccess"
	policyEC2FullAccess     = "arn:aws:iam::aws:policy/AmazonEC2FullAccess"

	defaultRoleName            = "kubernetes-v1"
	defaultInstanceProfileName = "kubernetes-v1"
	defaultSecurityGroupName   = "kubernetes-v1"

	maxRetries = 100
)

var (
	volumeTypes = sets.NewString(
		ec2.VolumeTypeStandard,
		ec2.VolumeTypeIo1,
		ec2.VolumeTypeGp2,
		ec2.VolumeTypeSc1,
		ec2.VolumeTypeSt1,
	)

	roleARNS = []string{policyRoute53FullAccess, policyEC2FullAccess}

	instanceProfileRole = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Service": "ec2.amazonaws.com"},
      "Action": "sts:AssumeRole"
    }
  ]
}`

	amiFilters = map[providerconfig.OperatingSystem]amiFilter{
		providerconfig.OperatingSystemCoreos: {
			description: "CoreOS Container Linux stable*",
			// The AWS marketplace ID from CoreOS
			owner: "595879546273",
		},
		providerconfig.OperatingSystemCentOS: {
			description: "CentOS Linux 7 x86_64 HVM EBS*",
			// The AWS marketplace ID from AWS
			owner: "679593333241",
		},
		providerconfig.OperatingSystemUbuntu: {
			// Be as precise as possible - otherwise we might get a nightly dev build
			description: "Canonical, Ubuntu, 18.04 LTS, amd64 bionic image build on ????-??-??",
			// The AWS marketplace ID from Canonical
			owner: "099720109477",
		},
	}
)

type RawConfig struct {
	AccessKeyID     providerconfig.ConfigVarString `json:"accessKeyId"`
	SecretAccessKey providerconfig.ConfigVarString `json:"secretAccessKey"`

	Region           providerconfig.ConfigVarString `json:"region"`
	AvailabilityZone providerconfig.ConfigVarString `json:"availabilityZone"`

	VpcID            providerconfig.ConfigVarString   `json:"vpcId"`
	SubnetID         providerconfig.ConfigVarString   `json:"subnetId"`
	SecurityGroupIDs []providerconfig.ConfigVarString `json:"securityGroupIDs"`
	InstanceProfile  providerconfig.ConfigVarString   `json:"instanceProfile"`

	InstanceType providerconfig.ConfigVarString `json:"instanceType"`
	AMI          providerconfig.ConfigVarString `json:"ami"`
	DiskSize     int64                          `json:"diskSize"`
	DiskType     providerconfig.ConfigVarString `json:"diskType"`
	Tags         map[string]string              `json:"tags"`
}

type Config struct {
	AccessKeyID     string
	SecretAccessKey string

	Region           string
	AvailabilityZone string

	VpcID            string
	SubnetID         string
	SecurityGroupIDs []string
	InstanceProfile  string

	InstanceType string
	AMI          string
	DiskSize     int64
	DiskType     string
	Tags         map[string]string
}

type amiFilter struct {
	description string
	owner       string
}

func getDefaultAMIID(client *ec2.EC2, os providerconfig.OperatingSystem) (string, error) {
	filter, osSupported := amiFilters[os]
	if !osSupported {
		return "", fmt.Errorf("operating system %q not supported", os)
	}

	imagesOut, err := client.DescribeImages(&ec2.DescribeImagesInput{
		Owners: aws.StringSlice([]string{filter.owner}),
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("description"),
				Values: aws.StringSlice([]string{filter.description}),
			},
			{
				Name:   aws.String("virtualization-type"),
				Values: aws.StringSlice([]string{"hvm"}),
			},
			{
				Name:   aws.String("root-device-type"),
				Values: aws.StringSlice([]string{"ebs"}),
			},
		},
	})
	if err != nil {
		return "", err
	}

	if len(imagesOut.Images) == 0 {
		return "", fmt.Errorf("could not find Image for '%s'", os)
	}

	image := imagesOut.Images[0]
	for _, v := range imagesOut.Images {
		itime, _ := time.Parse(time.RFC3339, *image.CreationDate)
		vtime, _ := time.Parse(time.RFC3339, *v.CreationDate)
		if vtime.After(itime) {
			image = v
		}
	}

	return *image.ImageId, nil
}

func getDefaultRootDevicePath(os providerconfig.OperatingSystem) (string, error) {
	switch os {
	case providerconfig.OperatingSystemUbuntu:
		return "/dev/sda1", nil
	case providerconfig.OperatingSystemCentOS:
		return "/dev/sda1", nil
	case providerconfig.OperatingSystemCoreos:
		return "/dev/xvda", nil
	}

	return "", fmt.Errorf("no default root path found for %s operating system", os)
}

func (p *provider) getConfig(s v1alpha1.ProviderConfig) (*Config, *providerconfig.Config, error) {
	if s.Value == nil {
		return nil, nil, fmt.Errorf("machine.spec.providerconfig.value is nil")
	}
	pconfig := providerconfig.Config{}
	err := json.Unmarshal(s.Value.Raw, &pconfig)
	if err != nil {
		return nil, nil, err
	}
	rawConfig := RawConfig{}
	err = json.Unmarshal(pconfig.CloudProviderSpec.Raw, &rawConfig)
	c := Config{}
	c.AccessKeyID, err = p.configVarResolver.GetConfigVarStringValueOrEnv(rawConfig.AccessKeyID, "AWS_ACCESS_KEY_ID")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"accessKeyId\" field, error = %v", err)
	}
	c.SecretAccessKey, err = p.configVarResolver.GetConfigVarStringValueOrEnv(rawConfig.SecretAccessKey, "AWS_SECRET_ACCESS_KEY")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get the value of \"secretAccessKey\" field, error = %v", err)
	}
	c.Region, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.Region)
	if err != nil {
		return nil, nil, err
	}
	c.VpcID, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.VpcID)
	if err != nil {
		return nil, nil, err
	}
	c.SubnetID, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.SubnetID)
	if err != nil {
		return nil, nil, err
	}
	c.AvailabilityZone, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.AvailabilityZone)
	if err != nil {
		return nil, nil, err
	}
	for _, securityGroupIDRaw := range rawConfig.SecurityGroupIDs {
		securityGroupID, err := p.configVarResolver.GetConfigVarStringValue(securityGroupIDRaw)
		if err != nil {
			return nil, nil, err
		}
		c.SecurityGroupIDs = append(c.SecurityGroupIDs, securityGroupID)
	}
	c.InstanceProfile, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.InstanceProfile)
	if err != nil {
		return nil, nil, err
	}
	c.InstanceType, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.InstanceType)
	if err != nil {
		return nil, nil, err
	}
	c.AMI, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.AMI)
	if err != nil {
		return nil, nil, err
	}
	c.DiskSize = rawConfig.DiskSize
	c.DiskType, err = p.configVarResolver.GetConfigVarStringValue(rawConfig.DiskType)
	if err != nil {
		return nil, nil, err
	}
	c.Tags = rawConfig.Tags

	return &c, &pconfig, err
}

func getSession(id, secret, token, region string) (*session.Session, error) {
	config := aws.NewConfig()
	config = config.WithRegion(region)
	config = config.WithCredentials(credentials.NewStaticCredentials(id, secret, token))
	config = config.WithMaxRetries(maxRetries)
	return session.NewSession(config)
}

func getIAMclient(id, secret, region string) (*iam.IAM, error) {
	sess, err := getSession(id, secret, "", region)
	if err != nil {
		return nil, awsErrorToTerminalError(err, "failed to get aws session")
	}
	return iam.New(sess), nil
}

func getEC2client(id, secret, region string) (*ec2.EC2, error) {
	sess, err := getSession(id, secret, "", region)
	if err != nil {
		return nil, awsErrorToTerminalError(err, "failed to get aws session")
	}
	return ec2.New(sess), nil
}

func (p *provider) AddDefaults(spec v1alpha1.MachineSpec) (v1alpha1.MachineSpec, bool, error) {
	return spec, false, nil
}

func (p *provider) Validate(spec v1alpha1.MachineSpec) error {
	config, pc, err := p.getConfig(spec.ProviderConfig)
	if err != nil {
		return fmt.Errorf("failed to parse config: %v", err)
	}

	if _, osSupported := amiFilters[pc.OperatingSystem]; !osSupported {
		return fmt.Errorf("unsupported os %s", pc.OperatingSystem)
	}

	if !volumeTypes.Has(config.DiskType) {
		return fmt.Errorf("invalid volume type %s specified. Supported: %s", config.DiskType, volumeTypes)
	}

	ec2Client, err := getEC2client(config.AccessKeyID, config.SecretAccessKey, config.Region)
	if err != nil {
		return fmt.Errorf("failed to create ec2 client: %v", err)
	}
	if config.AMI != "" {
		_, err := ec2Client.DescribeImages(&ec2.DescribeImagesInput{
			ImageIds: aws.StringSlice([]string{config.AMI}),
		})
		if err != nil {
			return fmt.Errorf("failed to validate ami: %v", err)
		}
	}

	if _, err := getVpc(ec2Client, config.VpcID); err != nil {
		return fmt.Errorf("invalid vpc %q specified: %v", config.VpcID, err)
	}

	_, err = ec2Client.DescribeAvailabilityZones(&ec2.DescribeAvailabilityZonesInput{ZoneNames: aws.StringSlice([]string{config.AvailabilityZone})})
	if err != nil {
		return fmt.Errorf("invalid zone %q specified: %v", config.AvailabilityZone, err)
	}

	_, err = ec2Client.DescribeRegions(&ec2.DescribeRegionsInput{RegionNames: aws.StringSlice([]string{config.Region})})
	if err != nil {
		return fmt.Errorf("invalid region %q specified: %v", config.Region, err)
	}

	if len(config.SecurityGroupIDs) > 0 {
		_, err := ec2Client.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
			GroupIds: aws.StringSlice(config.SecurityGroupIDs),
		})
		if err != nil {
			return fmt.Errorf("failed to validate security group id's: %v", err)
		}
	}

	iamClient, err := getIAMclient(config.AccessKeyID, config.SecretAccessKey, config.Region)
	if err != nil {
		return fmt.Errorf("failed to create iam client: %v", err)
	}

	if config.InstanceProfile != "" {
		_, err := iamClient.GetInstanceProfile(&iam.GetInstanceProfileInput{InstanceProfileName: aws.String(config.InstanceProfile)})
		if err != nil {
			return fmt.Errorf("failed to validate instance profile: %v", err)
		}
	}

	return nil
}

func getVpc(client *ec2.EC2, id string) (*ec2.Vpc, error) {
	vpcOut, err := client.DescribeVpcs(&ec2.DescribeVpcsInput{
		Filters: []*ec2.Filter{
			{Name: aws.String("vpc-id"), Values: []*string{aws.String(id)}},
		},
	})

	if err != nil {
		return nil, awsErrorToTerminalError(err, "failed to list vpc's")
	}

	if len(vpcOut.Vpcs) != 1 {
		return nil, fmt.Errorf("unable to find specified vpc with id %q", id)
	}

	return vpcOut.Vpcs[0], nil
}

func ensureDefaultSecurityGroupExists(client *ec2.EC2, vpc *ec2.Vpc) (string, error) {
	sgOut, err := client.DescribeSecurityGroups(&ec2.DescribeSecurityGroupsInput{
		GroupNames: aws.StringSlice([]string{defaultSecurityGroupName}),
	})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == "InvalidGroup.NotFound" {
				glog.V(4).Infof("creating security group %s...", defaultSecurityGroupName)
				csgOut, err := client.CreateSecurityGroup(&ec2.CreateSecurityGroupInput{
					VpcId:       vpc.VpcId,
					GroupName:   aws.String(defaultSecurityGroupName),
					Description: aws.String("Kubernetes security group"),
				})
				if err != nil {
					return "", awsErrorToTerminalError(err, "failed to create security group")
				}
				groupID := aws.StringValue(csgOut.GroupId)

				// Allow SSH from everywhere
				_, err = client.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
					CidrIp:     aws.String("0.0.0.0/0"),
					FromPort:   aws.Int64(22),
					ToPort:     aws.Int64(22),
					GroupId:    csgOut.GroupId,
					IpProtocol: aws.String("tcp"),
				})
				if err != nil {
					return "", awsErrorToTerminalError(err, fmt.Sprintf("failed to authorize security group ingress rule for ssh to security group %s", groupID))
				}

				// Allow kubelet 10250 from everywhere
				_, err = client.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
					CidrIp:     aws.String("0.0.0.0/0"),
					FromPort:   aws.Int64(10250),
					ToPort:     aws.Int64(10250),
					GroupId:    csgOut.GroupId,
					IpProtocol: aws.String("tcp"),
				})
				if err != nil {
					return "", awsErrorToTerminalError(err, fmt.Sprintf("failed to authorize security group ingress rule for kubelet port 10250 to security group %s", groupID))
				}

				// Allow node-to-node communication
				_, err = client.AuthorizeSecurityGroupIngress(&ec2.AuthorizeSecurityGroupIngressInput{
					SourceSecurityGroupName: aws.String(defaultSecurityGroupName),
					GroupId:                 csgOut.GroupId,
				})
				if err != nil {
					return "", awsErrorToTerminalError(err, fmt.Sprintf("failed to authorize security group ingress rule for node-to-node communication to security group %s", groupID))
				}

				glog.V(4).Infof("security group %s successfully created", defaultSecurityGroupName)
				return groupID, nil
			}
		}
		return "", awsErrorToTerminalError(err, "failed to list security group")
	}

	glog.V(6).Infof("security group %s already exists", defaultSecurityGroupName)
	return aws.StringValue(sgOut.SecurityGroups[0].GroupId), nil
}

func ensureDefaultRoleExists(client *iam.IAM) error {
	_, err := client.GetRole(&iam.GetRoleInput{RoleName: aws.String(defaultRoleName)})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == iam.ErrCodeNoSuchEntityException {
				glog.V(4).Infof("creating machine iam role %s...", defaultRoleName)
				paramsRole := &iam.CreateRoleInput{
					AssumeRolePolicyDocument: aws.String(instanceProfileRole),
					RoleName:                 aws.String(defaultRoleName),
				}
				_, err := client.CreateRole(paramsRole)
				if err != nil {
					return fmt.Errorf("failed to create role: %v", err)
				}

				for _, arn := range roleARNS {
					paramsAttachPolicy := &iam.AttachRolePolicyInput{
						PolicyArn: aws.String(arn),
						RoleName:  aws.String(defaultRoleName),
					}
					_, err = client.AttachRolePolicy(paramsAttachPolicy)
					if err != nil {
						return fmt.Errorf("failed to attach role %q to policy %q: %v", defaultRoleName, arn, err)
					}
				}
				glog.V(4).Infof("machine iam role %s successfully created", defaultRoleName)
				return nil
			}
			return awsErrorToTerminalError(err, fmt.Sprintf("failed to get role %s", defaultRoleName))
		}
		return fmt.Errorf("failed to get role %s: %v", defaultRoleName, err)
	}
	glog.V(6).Infof("machine iam role %s already exists", defaultRoleName)
	return nil
}

func ensureDefaultInstanceProfileExists(client *iam.IAM) error {
	err := ensureDefaultRoleExists(client)
	if err != nil {
		return err
	}

	_, err = client.GetInstanceProfile(&iam.GetInstanceProfileInput{InstanceProfileName: aws.String(defaultInstanceProfileName)})
	if err != nil {
		if awsErr, ok := err.(awserr.Error); ok {
			if awsErr.Code() == iam.ErrCodeNoSuchEntityException {
				glog.V(4).Infof("creating instance profile %s...", defaultInstanceProfileName)
				paramsInstanceProfile := &iam.CreateInstanceProfileInput{
					InstanceProfileName: aws.String(defaultInstanceProfileName),
				}
				_, err = client.CreateInstanceProfile(paramsInstanceProfile)
				if err != nil {
					return awsErrorToTerminalError(err, "failed to create instance profile")
				}

				paramsAddRole := &iam.AddRoleToInstanceProfileInput{
					InstanceProfileName: aws.String(defaultInstanceProfileName),
					RoleName:            aws.String(defaultRoleName),
				}
				_, err = client.AddRoleToInstanceProfile(paramsAddRole)
				if err != nil {
					return awsErrorToTerminalError(err, fmt.Sprintf("failed to add role %q to instance profile %q", defaultInstanceProfileName, defaultRoleName))
				}
				glog.V(4).Infof("instance profile %s successfully created", defaultInstanceProfileName)
				return nil
			}
			return awsErrorToTerminalError(err, fmt.Sprintf("failed to get instance profile %s", defaultInstanceProfileName))
		}
		return fmt.Errorf("failed to get instance profile: %v", err)
	}
	glog.V(6).Infof("instance profile %s already exists", defaultInstanceProfileName)

	return nil
}

func (p *provider) Create(machine *v1alpha1.Machine, update cloud.MachineUpdater, userdata string) (instance.Instance, error) {
	config, pc, err := p.getConfig(machine.Spec.ProviderConfig)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}

	ec2Client, err := getEC2client(config.AccessKeyID, config.SecretAccessKey, config.Region)
	if err != nil {
		return nil, err
	}

	iamClient, err := getIAMclient(config.AccessKeyID, config.SecretAccessKey, config.Region)
	if err != nil {
		return nil, err
	}

	instanceProfileName := config.InstanceProfile
	if instanceProfileName == "" {
		err = ensureDefaultInstanceProfileExists(iamClient)
		if err != nil {
			return nil, err
		}
		instanceProfileName = defaultInstanceProfileName
	}

	vpc, err := getVpc(ec2Client, config.VpcID)
	if err != nil {
		return nil, err
	}

	securityGroupIDs := config.SecurityGroupIDs
	if len(securityGroupIDs) == 0 {
		sgID, err := ensureDefaultSecurityGroupExists(ec2Client, vpc)
		if err != nil {
			return nil, err
		}
		securityGroupIDs = append(securityGroupIDs, sgID)
	}

	rootDevicePath, err := getDefaultRootDevicePath(pc.OperatingSystem)
	if err != nil {
		return nil, err
	}

	amiID := config.AMI
	if amiID == "" {
		if amiID, err = getDefaultAMIID(ec2Client, pc.OperatingSystem); err != nil {
			if err != nil {
				return nil, cloudprovidererrors.TerminalError{
					Reason:  common.InvalidConfigurationMachineError,
					Message: fmt.Sprintf("Invalid Region and Operating System configuration: %v", err),
				}
			}
		}
	}

	tags := []*ec2.Tag{
		{
			Key:   aws.String(nameTag),
			Value: aws.String(machine.Spec.Name),
		},
		{
			Key:   aws.String(machineUIDTag),
			Value: aws.String(string(machine.UID)),
		},
	}

	for k, v := range config.Tags {
		tags = append(tags, &ec2.Tag{
			Key:   aws.String(k),
			Value: aws.String(v),
		})
	}

	instanceRequest := &ec2.RunInstancesInput{
		ImageId: aws.String(amiID),
		BlockDeviceMappings: []*ec2.BlockDeviceMapping{
			{
				DeviceName: aws.String(rootDevicePath),
				Ebs: &ec2.EbsBlockDevice{
					VolumeSize:          aws.Int64(config.DiskSize),
					DeleteOnTermination: aws.Bool(true),
					VolumeType:          aws.String(config.DiskType),
				},
			},
		},
		MaxCount:     aws.Int64(1),
		MinCount:     aws.Int64(1),
		InstanceType: aws.String(config.InstanceType),
		UserData:     aws.String(base64.StdEncoding.EncodeToString([]byte(userdata))),
		Placement: &ec2.Placement{
			AvailabilityZone: aws.String(config.AvailabilityZone),
		},
		NetworkInterfaces: []*ec2.InstanceNetworkInterfaceSpecification{
			{
				DeviceIndex:              aws.Int64(0), // eth0
				AssociatePublicIpAddress: aws.Bool(true),
				DeleteOnTermination:      aws.Bool(true),
				SubnetId:                 aws.String(config.SubnetID),
			},
		},
		IamInstanceProfile: &ec2.IamInstanceProfileSpecification{
			Name: aws.String(instanceProfileName),
		},
		TagSpecifications: []*ec2.TagSpecification{
			{
				ResourceType: aws.String(ec2.ResourceTypeInstance),
				Tags:         tags,
			},
		},
	}

	runOut, err := ec2Client.RunInstances(instanceRequest)
	if err != nil {
		return nil, awsErrorToTerminalError(err, "failed create instance at aws")
	}
	awsInstance := &awsInstance{instance: runOut.Instances[0]}

	// Change to our security group
	_, err = ec2Client.ModifyInstanceAttribute(&ec2.ModifyInstanceAttributeInput{
		InstanceId: runOut.Instances[0].InstanceId,
		Groups:     aws.StringSlice(securityGroupIDs),
	})
	if err != nil {
		delErr := p.Delete(machine, update)
		if delErr != nil {
			return nil, awsErrorToTerminalError(err, fmt.Sprintf("failed to attach instance %s to security group %s & delete the created instance", aws.StringValue(runOut.Instances[0].InstanceId), defaultSecurityGroupName))
		}
		return nil, awsErrorToTerminalError(err, fmt.Sprintf("failed to attach instance %s to security group %s", aws.StringValue(runOut.Instances[0].InstanceId), defaultSecurityGroupName))
	}

	return awsInstance, nil
}

func (p *provider) Delete(machine *v1alpha1.Machine, _ cloud.MachineUpdater) error {
	instance, err := p.Get(machine)
	if err != nil {
		if err == cloudprovidererrors.ErrInstanceNotFound {
			return nil
		}
		return err
	}

	config, _, err := p.getConfig(machine.Spec.ProviderConfig)
	if err != nil {
		return cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}

	ec2Client, err := getEC2client(config.AccessKeyID, config.SecretAccessKey, config.Region)
	if err != nil {
		return err
	}

	tOut, err := ec2Client.TerminateInstances(&ec2.TerminateInstancesInput{
		InstanceIds: aws.StringSlice([]string{instance.ID()}),
	})
	if err != nil {
		return awsErrorToTerminalError(err, "failed to terminate instance")
	}

	if *tOut.TerminatingInstances[0].PreviousState.Name != *tOut.TerminatingInstances[0].CurrentState.Name {
		glog.V(4).Infof("successfully triggered termination of instance %s at aws", instance.ID())
	}

	return nil
}

func (p *provider) Get(machine *v1alpha1.Machine) (instance.Instance, error) {
	config, _, err := p.getConfig(machine.Spec.ProviderConfig)
	if err != nil {
		return nil, cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}

	ec2Client, err := getEC2client(config.AccessKeyID, config.SecretAccessKey, config.Region)
	if err != nil {
		return nil, err
	}

	inOut, err := ec2Client.DescribeInstances(&ec2.DescribeInstancesInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("tag:" + machineUIDTag),
				Values: aws.StringSlice([]string{string(machine.UID)}),
			},
		},
	})
	if err != nil {
		return nil, awsErrorToTerminalError(err, "failed to list instances from aws")
	}

	// We might have multiple instances (Maybe some old, terminated ones)
	// Thus we need to find the instance which is not in the terminated state
	for _, reservation := range inOut.Reservations {
		for _, i := range reservation.Instances {
			if i.State == nil || i.State.Name == nil {
				continue
			}

			if *i.State.Name == ec2.InstanceStateNameTerminated {
				continue
			}

			return &awsInstance{
				instance: i,
			}, nil
		}
	}

	return nil, cloudprovidererrors.ErrInstanceNotFound
}

func (p *provider) GetCloudConfig(spec v1alpha1.MachineSpec) (config string, name string, err error) {
	return "", "aws", nil
}

func (p *provider) MachineMetricsLabels(machine *v1alpha1.Machine) (map[string]string, error) {
	labels := make(map[string]string)

	c, _, err := p.getConfig(machine.Spec.ProviderConfig)
	if err == nil {
		labels["size"] = c.InstanceType
		labels["region"] = c.Region
		labels["az"] = c.AvailabilityZone
		labels["ami"] = c.AMI
	}

	return labels, err
}

func (p *provider) MigrateUID(machine *v1alpha1.Machine, new types.UID) error {
	instance, err := p.Get(machine)
	if err != nil {
		if err == cloudprovidererrors.ErrInstanceNotFound {
			return nil
		}
		return fmt.Errorf("failed to get instance: %v", err)
	}

	config, _, err := p.getConfig(machine.Spec.ProviderConfig)
	if err != nil {
		return cloudprovidererrors.TerminalError{
			Reason:  common.InvalidConfigurationMachineError,
			Message: fmt.Sprintf("Failed to parse MachineSpec, due to %v", err),
		}
	}

	ec2Client, err := getEC2client(config.AccessKeyID, config.SecretAccessKey, config.Region)
	if err != nil {
		return fmt.Errorf("failed to get EC2 client: %v", err)
	}

	_, err = ec2Client.CreateTags(&ec2.CreateTagsInput{
		Resources: aws.StringSlice([]string{instance.ID()}),
		Tags:      []*ec2.Tag{{Key: aws.String(machineUIDTag), Value: aws.String(string(new))}}})
	if err != nil {
		return fmt.Errorf("failed to update instance with new machineUIDTag: %v", err)
	}

	return nil
}

type awsInstance struct {
	instance *ec2.Instance
}

func (d *awsInstance) Name() string {
	return getTagValue(nameTag, d.instance.Tags)
}

func (d *awsInstance) ID() string {
	return aws.StringValue(d.instance.InstanceId)
}

func (d *awsInstance) Addresses() []string {
	return []string{
		aws.StringValue(d.instance.PublicIpAddress),
		aws.StringValue(d.instance.PublicDnsName),
		aws.StringValue(d.instance.PrivateIpAddress),
		aws.StringValue(d.instance.PrivateDnsName),
	}
}

func (d *awsInstance) Status() instance.Status {
	switch *d.instance.State.Name {
	case ec2.InstanceStateNameRunning:
		return instance.StatusRunning
	case ec2.InstanceStateNamePending:
		return instance.StatusCreating
	case ec2.InstanceStateNameTerminated:
		return instance.StatusDeleted
	case ec2.InstanceStateNameShuttingDown:
		return instance.StatusDeleting
	default:
		return instance.StatusUnknown
	}
}

func getTagValue(name string, tags []*ec2.Tag) string {
	for _, t := range tags {
		if *t.Key == name {
			return *t.Value
		}
	}
	return ""
}

// awsErrorToTerminalError judges if the given error
// can be qualified as a "terminal" error, for more info see v1alpha1.MachineStatus
//
// if the given error doesn't qualify the error passed as
// an argument will be formatted according to msg and returned
func awsErrorToTerminalError(err error, msg string) error {
	prepareAndReturnError := func() error {
		return fmt.Errorf("%s, due to %s", msg, err)
	}

	if err != nil {
		aerr, ok := err.(awserr.Error)
		if !ok {
			return prepareAndReturnError()
		}
		switch aerr.Code() {
		case "InstanceLimitExceeded":
			return cloudprovidererrors.TerminalError{
				Reason:  common.InsufficientResourcesMachineError,
				Message: "You've reached the AWS quota for number of instances of this type",
			}
		case "AuthFailure":
			// authorization primitives come from MachineSpec
			// thus we are setting InvalidConfigurationMachineError
			return cloudprovidererrors.TerminalError{
				Reason:  common.InvalidConfigurationMachineError,
				Message: "A request has been rejected due to invalid credentials which were taken from the MachineSpec",
			}
		default:
			return prepareAndReturnError()
		}
	}
	return err
}
