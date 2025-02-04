/*
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

package launchtemplate_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/ec2"
	admv1alpha1 "github.com/awslabs/amazon-eks-ami/nodeadm/api/v1alpha1"
	opstatus "github.com/awslabs/operatorpkg/status"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	clock "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	corev1beta1 "sigs.k8s.io/karpenter/pkg/apis/v1beta1"
	corecloudprovider "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/controllers/provisioning"
	"sigs.k8s.io/karpenter/pkg/controllers/state"
	"sigs.k8s.io/karpenter/pkg/events"
	coreoptions "sigs.k8s.io/karpenter/pkg/operator/options"
	"sigs.k8s.io/karpenter/pkg/operator/scheme"
	coretest "sigs.k8s.io/karpenter/pkg/test"

	. "sigs.k8s.io/karpenter/pkg/test/expectations"
	. "sigs.k8s.io/karpenter/pkg/utils/testing"

	"github.com/aws/karpenter-provider-aws/pkg/apis"
	"github.com/aws/karpenter-provider-aws/pkg/apis/v1beta1"
	"github.com/aws/karpenter-provider-aws/pkg/cloudprovider"
	"github.com/aws/karpenter-provider-aws/pkg/controllers/nodeclass/status"
	"github.com/aws/karpenter-provider-aws/pkg/fake"
	"github.com/aws/karpenter-provider-aws/pkg/operator/options"
	"github.com/aws/karpenter-provider-aws/pkg/providers/amifamily"
	"github.com/aws/karpenter-provider-aws/pkg/providers/amifamily/bootstrap"
	"github.com/aws/karpenter-provider-aws/pkg/providers/amifamily/bootstrap/mime"
	"github.com/aws/karpenter-provider-aws/pkg/providers/instancetype"
	"github.com/aws/karpenter-provider-aws/pkg/providers/launchtemplate"
	"github.com/aws/karpenter-provider-aws/pkg/test"
)

var ctx context.Context
var stop context.CancelFunc
var env *coretest.Environment
var awsEnv *test.Environment
var fakeClock *clock.FakeClock
var prov *provisioning.Provisioner
var cluster *state.Cluster
var cloudProvider *cloudprovider.CloudProvider

func TestAWS(t *testing.T) {
	ctx = TestContextWithLogger(t)
	RegisterFailHandler(Fail)
	RunSpecs(t, "LaunchTemplateProvider")
}

var _ = BeforeSuite(func() {
	env = coretest.NewEnvironment(scheme.Scheme, coretest.WithCRDs(apis.CRDs...))
	ctx = coreoptions.ToContext(ctx, coretest.Options())
	ctx = options.ToContext(ctx, test.Options())
	ctx, stop = context.WithCancel(ctx)
	awsEnv = test.NewEnvironment(ctx, env)

	fakeClock = &clock.FakeClock{}
	cloudProvider = cloudprovider.New(awsEnv.InstanceTypesProvider, awsEnv.InstanceProvider, events.NewRecorder(&record.FakeRecorder{}),
		env.Client, awsEnv.AMIProvider, awsEnv.SecurityGroupProvider, awsEnv.SubnetProvider)
	cluster = state.NewCluster(fakeClock, env.Client, cloudProvider)
	prov = provisioning.NewProvisioner(env.Client, events.NewRecorder(&record.FakeRecorder{}), cloudProvider, cluster)
})

var _ = AfterSuite(func() {
	stop()
	Expect(env.Stop()).To(Succeed(), "Failed to stop environment")
})

var _ = BeforeEach(func() {
	ctx = coreoptions.ToContext(ctx, coretest.Options())
	ctx = options.ToContext(ctx, test.Options())
	cluster.Reset()
	awsEnv.Reset()

	awsEnv.LaunchTemplateProvider.KubeDNSIP = net.ParseIP("10.0.100.10")
	awsEnv.LaunchTemplateProvider.ClusterEndpoint = "https://test-cluster"
	awsEnv.LaunchTemplateProvider.CABundle = lo.ToPtr("ca-bundle")
})

var _ = AfterEach(func() {
	ExpectCleanedUp(ctx, env.Client)
})

var _ = Describe("LaunchTemplate Provider", func() {
	var nodePool *corev1beta1.NodePool
	var nodeClass *v1beta1.EC2NodeClass
	BeforeEach(func() {
		nodeClass = test.EC2NodeClass(
			v1beta1.EC2NodeClass{
				Status: v1beta1.EC2NodeClassStatus{
					InstanceProfile: "test-profile",
					SecurityGroups: []v1beta1.SecurityGroup{
						{
							ID: "sg-test1",
						},
						{
							ID: "sg-test2",
						},
						{
							ID: "sg-test3",
						},
					},
					Subnets: []v1beta1.Subnet{
						{
							ID:   "subnet-test1",
							Zone: "test-zone-1a",
						},
						{
							ID:   "subnet-test2",
							Zone: "test-zone-1b",
						},
						{
							ID:   "subnet-test3",
							Zone: "test-zone-1c",
						},
					},
				},
			},
		)
		nodeClass.StatusConditions().SetTrue(opstatus.ConditionReady)
		nodePool = coretest.NodePool(corev1beta1.NodePool{
			Spec: corev1beta1.NodePoolSpec{
				Template: corev1beta1.NodeClaimTemplate{
					ObjectMeta: corev1beta1.ObjectMeta{
						// TODO @joinnis: Move this into the coretest.NodePool function
						Labels: map[string]string{coretest.DiscoveryLabel: "unspecified"},
					},
					Spec: corev1beta1.NodeClaimSpec{
						Requirements: []corev1beta1.NodeSelectorRequirementWithMinValues{
							{
								NodeSelectorRequirement: v1.NodeSelectorRequirement{
									Key:      corev1beta1.CapacityTypeLabelKey,
									Operator: v1.NodeSelectorOpIn,
									Values:   []string{corev1beta1.CapacityTypeOnDemand},
								},
							},
						},
						Kubelet: &corev1beta1.KubeletConfiguration{},
						NodeClassRef: &corev1beta1.NodeClassReference{
							Name: nodeClass.Name,
						},
					},
				},
			},
		})
		_, err := awsEnv.SubnetProvider.List(ctx, nodeClass) // Hydrate the subnet cache
		Expect(err).To(BeNil())
		Expect(awsEnv.InstanceTypesProvider.UpdateInstanceTypes(ctx)).To(Succeed())
		Expect(awsEnv.InstanceTypesProvider.UpdateInstanceTypeOfferings(ctx)).To(Succeed())
	})
	It("should create unique launch templates for multiple identical nodeClasses", func() {
		nodeClass2 := test.EC2NodeClass(v1beta1.EC2NodeClass{
			Status: v1beta1.EC2NodeClassStatus{
				InstanceProfile: "test-profile",
				Subnets:         nodeClass.Status.Subnets,
				SecurityGroups:  nodeClass.Status.SecurityGroups,
				AMIs:            nodeClass.Status.AMIs,
			},
		})
		_, err := awsEnv.SubnetProvider.List(ctx, nodeClass2) // Hydrate the subnet cache
		Expect(err).To(BeNil())
		nodePool2 := coretest.NodePool(corev1beta1.NodePool{
			Spec: corev1beta1.NodePoolSpec{
				Template: corev1beta1.NodeClaimTemplate{
					Spec: corev1beta1.NodeClaimSpec{
						Requirements: []corev1beta1.NodeSelectorRequirementWithMinValues{
							{
								NodeSelectorRequirement: v1.NodeSelectorRequirement{
									Key:      corev1beta1.CapacityTypeLabelKey,
									Operator: v1.NodeSelectorOpIn,
									Values:   []string{corev1beta1.CapacityTypeSpot},
								},
							},
						},
						NodeClassRef: &corev1beta1.NodeClassReference{
							Name: nodeClass2.Name,
						},
					},
				},
			},
		})
		nodeClass2.Status.SecurityGroups = []v1beta1.SecurityGroup{
			{
				ID: "sg-test1",
			},
			{
				ID: "sg-test2",
			},
			{
				ID: "sg-test3",
			},
		}
		nodeClass2.Status.Subnets = []v1beta1.Subnet{
			{
				ID:   "subnet-test1",
				Zone: "test-zone-1a",
			},
			{
				ID:   "subnet-test2",
				Zone: "test-zone-1b",
			},
			{
				ID:   "subnet-test3",
				Zone: "test-zone-1c",
			},
		}
		nodeClass2.StatusConditions().SetTrue(opstatus.ConditionReady)

		pods := []*v1.Pod{
			coretest.UnschedulablePod(coretest.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
				{
					Key:      corev1beta1.CapacityTypeLabelKey,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{corev1beta1.CapacityTypeSpot},
				},
			},
			}),
			coretest.UnschedulablePod(coretest.PodOptions{NodeRequirements: []v1.NodeSelectorRequirement{
				{
					Key:      corev1beta1.CapacityTypeLabelKey,
					Operator: v1.NodeSelectorOpIn,
					Values:   []string{corev1beta1.CapacityTypeOnDemand},
				},
			},
			}),
		}
		ExpectApplied(ctx, env.Client, nodePool, nodeClass, nodePool2, nodeClass2)
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
		ltConfigCount := len(awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.Pop().LaunchTemplateConfigs) + len(awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.Pop().LaunchTemplateConfigs)
		Expect(ltConfigCount).To(BeNumerically("==", awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()))
		nodeClasses := [2]string{nodeClass.Name, nodeClass2.Name}
		awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
			for _, value := range ltInput.LaunchTemplateData.TagSpecifications[0].Tags {
				if *value.Key == v1beta1.LabelNodeClass {
					Expect(*value.Value).To(BeElementOf(nodeClasses))
				}
			}
		})
	})
	It("should default to a generated launch template", func() {
		ExpectApplied(ctx, env.Client, nodePool, nodeClass)
		pod := coretest.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectScheduled(ctx, env.Client, pod)

		Expect(awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.Len()).To(BeNumerically("==", 1))
		createFleetInput := awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.Pop()
		Expect(len(createFleetInput.LaunchTemplateConfigs)).To(BeNumerically("==", awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()))
		Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
		awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
			launchTemplate, ok := lo.Find(createFleetInput.LaunchTemplateConfigs, func(ltConfig *ec2.FleetLaunchTemplateConfigRequest) bool {
				return *ltConfig.LaunchTemplateSpecification.LaunchTemplateName == *ltInput.LaunchTemplateName
			})
			Expect(ok).To(BeTrue())
			Expect(ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.Encrypted).To(Equal(aws.Bool(true)))
			Expect(*launchTemplate.LaunchTemplateSpecification.Version).To(Equal("$Latest"))
		})
	})
	It("should fail to provision if the instance profile isn't defined", func() {
		nodeClass.Status.InstanceProfile = ""
		ExpectApplied(ctx, env.Client, nodePool, nodeClass)
		pod := coretest.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectNotScheduled(ctx, env.Client, pod)
	})
	It("should use the instance profile on the EC2NodeClass when specified", func() {
		nodeClass.Spec.Role = ""
		nodeClass.Spec.InstanceProfile = aws.String("overridden-profile")
		ExpectApplied(ctx, env.Client, nodePool, nodeClass)
		pod := coretest.UnschedulablePod()
		ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
		ExpectScheduled(ctx, env.Client, pod)
		Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
		awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
			Expect(*ltInput.LaunchTemplateData.IamInstanceProfile.Name).To(Equal("overridden-profile"))
		})
	})
	Context("Cache", func() {
		It("should use same launch template for equivalent constraints", func() {
			t1 := v1.Toleration{
				Key:      "Abacus",
				Operator: "Equal",
				Value:    "Zebra",
				Effect:   "NoSchedule",
			}
			t2 := v1.Toleration{
				Key:      "Zebra",
				Operator: "Equal",
				Value:    "Abacus",
				Effect:   "NoSchedule",
			}
			t3 := v1.Toleration{
				Key:      "Boar",
				Operator: "Equal",
				Value:    "Abacus",
				Effect:   "NoSchedule",
			}

			// constrain the packer to a single launch template type
			rr := v1.ResourceRequirements{
				Requests: v1.ResourceList{
					v1.ResourceCPU:            resource.MustParse("24"),
					v1beta1.ResourceNVIDIAGPU: resource.MustParse("1"),
				},
				Limits: v1.ResourceList{v1beta1.ResourceNVIDIAGPU: resource.MustParse("1")},
			}

			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod1 := coretest.UnschedulablePod(coretest.PodOptions{
				Tolerations:          []v1.Toleration{t1, t2, t3},
				ResourceRequirements: rr,
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod1)
			ExpectScheduled(ctx, env.Client, pod1)
			Expect(awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.Len()).To(Equal(1))
			createFleetInput := awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.Pop()
			lts1 := sets.NewString()
			for _, ltConfig := range createFleetInput.LaunchTemplateConfigs {
				lts1.Insert(*ltConfig.LaunchTemplateSpecification.LaunchTemplateName)
			}

			pod2 := coretest.UnschedulablePod(coretest.PodOptions{
				Tolerations:          []v1.Toleration{t2, t3, t1},
				ResourceRequirements: rr,
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod2)

			ExpectScheduled(ctx, env.Client, pod2)
			Expect(awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.Len()).To(Equal(1))
			createFleetInput = awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.Pop()
			lts2 := sets.NewString()
			for _, ltConfig := range createFleetInput.LaunchTemplateConfigs {
				lts2.Insert(*ltConfig.LaunchTemplateSpecification.LaunchTemplateName)
			}
			Expect(lts1.Equal(lts2)).To(BeTrue())
		})
		It("should recover from an out-of-sync launch template cache", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{MaxPods: aws.Int32(1)}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				ltName := aws.StringValue(ltInput.LaunchTemplateName)
				lt, ok := awsEnv.LaunchTemplateCache.Get(ltName)
				Expect(ok).To(Equal(true))
				// Remove expiration from cached LT
				awsEnv.LaunchTemplateCache.Set(ltName, lt, -1)
			})
			awsEnv.EC2API.CreateFleetBehavior.Error.Set(awserr.New("InvalidLaunchTemplateName.NotFoundException", "", nil), fake.MaxCalls(1))
			pod = coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			// should call fleet twice. Once will fail on invalid LT and the next will succeed
			Expect(awsEnv.EC2API.CreateFleetBehavior.FailedCalls()).To(BeNumerically("==", 1))
			Expect(awsEnv.EC2API.CreateFleetBehavior.SuccessfulCalls()).To(BeNumerically("==", 2))

		})
		// Testing launch template hash key will produce unique hashes
		It("should generate different launch template names based on amifamily option configuration", func() {
			options := []*amifamily.Options{
				{},
				{ClusterName: "test-name"},
				{ClusterEndpoint: "test-endpoint"},
				{ClusterCIDR: lo.ToPtr("test-cidr")},
				{InstanceProfile: "test-profile"},
				{InstanceStorePolicy: lo.ToPtr(v1beta1.InstanceStorePolicyRAID0)},
				{SecurityGroups: []v1beta1.SecurityGroup{{Name: "test-sg"}}},
				{Tags: map[string]string{"test-key": "test-value"}},
				{KubeDNSIP: net.ParseIP("192.0.0.2")},
				{AssociatePublicIPAddress: lo.ToPtr(true)},
				{NodeClassName: "test-name"},
			}
			launchtemplateResult := []string{}
			for _, option := range options {
				lt := &amifamily.LaunchTemplate{Options: option}
				launchtemplateResult = append(launchtemplateResult, launchtemplate.LaunchTemplateName(lt))
			}
			Expect(len(launchtemplateResult)).To(BeNumerically("==", 11))
			Expect(lo.Uniq(launchtemplateResult)).To(Equal(launchtemplateResult))
		})
		It("should not generate different launch template names based on CABundle and Labels", func() {
			options := []*amifamily.Options{
				{},
				{CABundle: lo.ToPtr("test-bundle")},
				{Labels: map[string]string{"test-key": "test-value"}},
			}
			launchtemplateResult := []string{}
			for _, option := range options {
				lt := &amifamily.LaunchTemplate{Options: option}
				launchtemplateResult = append(launchtemplateResult, launchtemplate.LaunchTemplateName(lt))
			}
			Expect(len(lo.Uniq(launchtemplateResult))).To(BeNumerically("==", 1))
			Expect(lo.Uniq(launchtemplateResult)[0]).To(Equal(launchtemplate.LaunchTemplateName(&amifamily.LaunchTemplate{Options: &amifamily.Options{}})))
		})
		It("should generate different launch template names based on kubelet configuration", func() {
			kubeletChanges := []*corev1beta1.KubeletConfiguration{
				{},
				{KubeReserved: map[string]string{string(v1.ResourceCPU): "20"}},
				{SystemReserved: map[string]string{string(v1.ResourceMemory): "10Gi"}},
				{EvictionHard: map[string]string{"memory.available": "52%"}},
				{EvictionSoft: map[string]string{"nodefs.available": "132%"}},
				{MaxPods: aws.Int32(20)},
			}
			launchtemplateResult := []string{}
			for _, kubelet := range kubeletChanges {
				lt := &amifamily.LaunchTemplate{UserData: bootstrap.EKS{Options: bootstrap.Options{KubeletConfig: kubelet}}}
				launchtemplateResult = append(launchtemplateResult, launchtemplate.LaunchTemplateName(lt))
			}
			Expect(len(launchtemplateResult)).To(BeNumerically("==", 6))
			Expect(lo.Uniq(launchtemplateResult)).To(Equal(launchtemplateResult))
		})
		It("should generate different launch template names based on bootstrap configuration", func() {
			bootstrapOptions := []*bootstrap.Options{
				{},
				{ClusterName: "test-name"},
				{ClusterEndpoint: "test-endpoint"},
				{ClusterCIDR: lo.ToPtr("test-cidr")},
				{Taints: []v1.Taint{{Key: "test-key", Value: "test-value"}}},
				{Labels: map[string]string{"test-key": "test-value"}},
				{CABundle: lo.ToPtr("test-bundle")},
				{AWSENILimitedPodDensity: true},
				{ContainerRuntime: lo.ToPtr("test-cri")},
				{CustomUserData: lo.ToPtr("test-cidr")},
			}
			launchtemplateResult := []string{}
			for _, option := range bootstrapOptions {
				lt := &amifamily.LaunchTemplate{UserData: bootstrap.EKS{Options: *option}}
				launchtemplateResult = append(launchtemplateResult, launchtemplate.LaunchTemplateName(lt))
			}
			Expect(len(launchtemplateResult)).To(BeNumerically("==", 10))
			Expect(lo.Uniq(launchtemplateResult)).To(Equal(launchtemplateResult))
		})
		It("should generate different launch template names based on launchtemplate option configuration", func() {
			launchtemplates := []*amifamily.LaunchTemplate{
				{},
				{BlockDeviceMappings: []*v1beta1.BlockDeviceMapping{{DeviceName: lo.ToPtr("test-block")}}},
				{AMIID: "test-ami"},
				{DetailedMonitoring: true},
				{EFACount: 12},
				{CapacityType: "spot"},
			}
			launchtemplateResult := []string{}
			for _, lt := range launchtemplates {
				launchtemplateResult = append(launchtemplateResult, launchtemplate.LaunchTemplateName(lt))
			}
			Expect(len(launchtemplateResult)).To(BeNumerically("==", 6))
			Expect(lo.Uniq(launchtemplateResult)).To(Equal(launchtemplateResult))
		})
		It("should not generate different launch template names based on instance types", func() {
			launchtemplates := []*amifamily.LaunchTemplate{
				{},
				{InstanceTypes: []*corecloudprovider.InstanceType{{Name: "test-instance-type"}}},
			}
			launchtemplateResult := []string{}
			for _, lt := range launchtemplates {
				launchtemplateResult = append(launchtemplateResult, launchtemplate.LaunchTemplateName(lt))
			}
			Expect(len(lo.Uniq(launchtemplateResult))).To(BeNumerically("==", 1))
			Expect(lo.Uniq(launchtemplateResult)[0]).To(Equal(launchtemplate.LaunchTemplateName(&amifamily.LaunchTemplate{})))
		})
	})
	Context("Labels", func() {
		It("should apply labels to the node", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels).To(HaveKey(v1.LabelOSStable))
			Expect(node.Labels).To(HaveKey(v1.LabelArchStable))
			Expect(node.Labels).To(HaveKey(v1.LabelInstanceTypeStable))
		})
	})
	Context("Tags", func() {
		It("should request that tags be applied to both instances and volumes", func() {
			nodeClass.Spec.Tags = map[string]string{
				"tag1": "tag1value",
				"tag2": "tag2value",
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.Len()).To(Equal(1))
			createFleetInput := awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.Pop()
			Expect(createFleetInput.TagSpecifications).To(HaveLen(3))

			// tags should be included in instance, volume, and fleet tag specification
			Expect(*createFleetInput.TagSpecifications[0].ResourceType).To(Equal(ec2.ResourceTypeInstance))
			ExpectTags(createFleetInput.TagSpecifications[0].Tags, nodeClass.Spec.Tags)

			Expect(*createFleetInput.TagSpecifications[1].ResourceType).To(Equal(ec2.ResourceTypeVolume))
			ExpectTags(createFleetInput.TagSpecifications[1].Tags, nodeClass.Spec.Tags)

			Expect(*createFleetInput.TagSpecifications[2].ResourceType).To(Equal(ec2.ResourceTypeFleet))
			ExpectTags(createFleetInput.TagSpecifications[2].Tags, nodeClass.Spec.Tags)
		})
		It("should request that tags be applied to both network interfaces and spot instance requests", func() {
			nodeClass.Spec.Tags = map[string]string{
				"tag1": "tag1value",
				"tag2": "tag2value",
			}
			nodePool.Spec.Template.Spec.Requirements = []corev1beta1.NodeSelectorRequirementWithMinValues{
				{
					NodeSelectorRequirement: v1.NodeSelectorRequirement{
						Key:      corev1beta1.CapacityTypeLabelKey,
						Operator: v1.NodeSelectorOpIn,
						Values:   []string{corev1beta1.CapacityTypeSpot},
					},
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(i *ec2.CreateLaunchTemplateInput) {
				Expect(i.LaunchTemplateData.TagSpecifications).To(HaveLen(2))

				// tags should be included in instance, volume, and fleet tag specification
				Expect(*i.LaunchTemplateData.TagSpecifications[0].ResourceType).To(Equal(ec2.ResourceTypeNetworkInterface))
				ExpectTags(i.LaunchTemplateData.TagSpecifications[0].Tags, nodeClass.Spec.Tags)

				Expect(*i.LaunchTemplateData.TagSpecifications[1].ResourceType).To(Equal(ec2.ResourceTypeSpotInstancesRequest))
				ExpectTags(i.LaunchTemplateData.TagSpecifications[1].Tags, nodeClass.Spec.Tags)
			})
		})
		It("should override default tag names", func() {
			// these tags are defaulted, so ensure users can override them
			nodeClass.Spec.Tags = map[string]string{
				"Name": "myname",
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.Len()).To(Equal(1))
			createFleetInput := awsEnv.EC2API.CreateFleetBehavior.CalledWithInput.Pop()
			Expect(createFleetInput.TagSpecifications).To(HaveLen(3))

			// tags should be included in instance, volume, and fleet tag specification
			Expect(*createFleetInput.TagSpecifications[0].ResourceType).To(Equal(ec2.ResourceTypeInstance))
			ExpectTags(createFleetInput.TagSpecifications[0].Tags, nodeClass.Spec.Tags)

			Expect(*createFleetInput.TagSpecifications[1].ResourceType).To(Equal(ec2.ResourceTypeVolume))
			ExpectTags(createFleetInput.TagSpecifications[1].Tags, nodeClass.Spec.Tags)

			Expect(*createFleetInput.TagSpecifications[2].ResourceType).To(Equal(ec2.ResourceTypeFleet))
			ExpectTags(createFleetInput.TagSpecifications[2].Tags, nodeClass.Spec.Tags)
		})
	})
	Context("Block Device Mappings", func() {
		It("should default AL2 block device mappings", func() {
			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyAL2
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				Expect(len(ltInput.LaunchTemplateData.BlockDeviceMappings)).To(Equal(1))
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.VolumeSize).To(Equal(int64(20)))
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.VolumeType).To(Equal("gp3"))
				Expect(ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.Iops).To(BeNil())
			})
		})
		It("should default AL2023 block device mappings", func() {
			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyAL2023
			awsEnv.LaunchTemplateProvider.CABundle = lo.ToPtr("Y2EtYnVuZGxlCg==")
			awsEnv.LaunchTemplateProvider.ClusterCIDR.Store(lo.ToPtr("10.100.0.0/16"))
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				Expect(len(ltInput.LaunchTemplateData.BlockDeviceMappings)).To(Equal(1))
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.VolumeSize).To(Equal(int64(20)))
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.VolumeType).To(Equal("gp3"))
				Expect(ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.Iops).To(BeNil())
			})
		})
		It("should use custom block device mapping", func() {
			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyAL2
			nodeClass.Spec.BlockDeviceMappings = []*v1beta1.BlockDeviceMapping{
				{
					DeviceName: aws.String("/dev/xvda"),
					EBS: &v1beta1.BlockDevice{
						DeleteOnTermination: aws.Bool(true),
						Encrypted:           aws.Bool(true),
						VolumeType:          aws.String("io2"),
						VolumeSize:          lo.ToPtr(resource.MustParse("200G")),
						IOPS:                aws.Int64(10_000),
						KMSKeyID:            aws.String("arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"),
					},
				},
				{
					DeviceName: aws.String("/dev/xvdb"),
					EBS: &v1beta1.BlockDevice{
						DeleteOnTermination: aws.Bool(true),
						Encrypted:           aws.Bool(true),
						VolumeType:          aws.String("io2"),
						VolumeSize:          lo.ToPtr(resource.MustParse("200Gi")),
						IOPS:                aws.Int64(10_000),
						KMSKeyID:            aws.String("arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"),
					},
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				Expect(ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs).To(Equal(&ec2.LaunchTemplateEbsBlockDeviceRequest{
					VolumeSize:          aws.Int64(187),
					VolumeType:          aws.String("io2"),
					Iops:                aws.Int64(10_000),
					DeleteOnTermination: aws.Bool(true),
					Encrypted:           aws.Bool(true),
					KmsKeyId:            aws.String("arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"),
				}))
				Expect(ltInput.LaunchTemplateData.BlockDeviceMappings[1].Ebs).To(Equal(&ec2.LaunchTemplateEbsBlockDeviceRequest{
					VolumeSize:          aws.Int64(200),
					VolumeType:          aws.String("io2"),
					Iops:                aws.Int64(10_000),
					DeleteOnTermination: aws.Bool(true),
					Encrypted:           aws.Bool(true),
					KmsKeyId:            aws.String("arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"),
				}))
			})
		})
		It("should round up for custom block device mappings when specified in gigabytes", func() {
			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyAL2
			nodeClass.Spec.BlockDeviceMappings = []*v1beta1.BlockDeviceMapping{
				{
					DeviceName: aws.String("/dev/xvda"),
					EBS: &v1beta1.BlockDevice{
						DeleteOnTermination: aws.Bool(true),
						Encrypted:           aws.Bool(true),
						VolumeType:          aws.String("io2"),
						VolumeSize:          lo.ToPtr(resource.MustParse("4G")),
						IOPS:                aws.Int64(10_000),
						KMSKeyID:            aws.String("arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"),
					},
				},
				{
					DeviceName: aws.String("/dev/xvdb"),
					EBS: &v1beta1.BlockDevice{
						DeleteOnTermination: aws.Bool(true),
						Encrypted:           aws.Bool(true),
						VolumeType:          aws.String("io2"),
						VolumeSize:          lo.ToPtr(resource.MustParse("2G")),
						IOPS:                aws.Int64(10_000),
						KMSKeyID:            aws.String("arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"),
					},
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				// Both of these values are rounded up when converting to Gibibytes
				Expect(aws.Int64Value(ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.VolumeSize)).To(BeNumerically("==", 4))
				Expect(aws.Int64Value(ltInput.LaunchTemplateData.BlockDeviceMappings[1].Ebs.VolumeSize)).To(BeNumerically("==", 2))
			})
		})
		It("should default bottlerocket second volume with root volume size", func() {
			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyBottlerocket
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				Expect(len(ltInput.LaunchTemplateData.BlockDeviceMappings)).To(Equal(2))
				// Bottlerocket control volume
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.VolumeSize).To(Equal(int64(4)))
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.VolumeType).To(Equal("gp3"))
				Expect(ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.Iops).To(BeNil())
				// Bottlerocket user volume
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[1].Ebs.VolumeSize).To(Equal(int64(20)))
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[1].Ebs.VolumeType).To(Equal("gp3"))
				Expect(ltInput.LaunchTemplateData.BlockDeviceMappings[1].Ebs.Iops).To(BeNil())
			})
		})
		It("should not default block device mappings for custom AMIFamilies", func() {
			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyCustom
			nodeClass.Spec.AMISelectorTerms = []v1beta1.AMISelectorTerm{{Tags: map[string]string{"*": "*"}}}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				Expect(len(ltInput.LaunchTemplateData.BlockDeviceMappings)).To(Equal(0))
			})
		})
		It("should use custom block device mapping for custom AMIFamilies", func() {
			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyCustom
			nodeClass.Spec.AMISelectorTerms = []v1beta1.AMISelectorTerm{{Tags: map[string]string{"*": "*"}}}
			nodeClass.Spec.BlockDeviceMappings = []*v1beta1.BlockDeviceMapping{
				{
					DeviceName: aws.String("/dev/xvda"),
					EBS: &v1beta1.BlockDevice{
						DeleteOnTermination: aws.Bool(true),
						Encrypted:           aws.Bool(true),
						VolumeType:          aws.String("io2"),
						VolumeSize:          lo.ToPtr(resource.MustParse("40Gi")),
						IOPS:                aws.Int64(10_000),
						KMSKeyID:            aws.String("arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"),
					},
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				Expect(len(ltInput.LaunchTemplateData.BlockDeviceMappings)).To(Equal(1))
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.VolumeSize).To(Equal(int64(40)))
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.VolumeType).To(Equal("io2"))
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.Iops).To(Equal(int64(10_000)))
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.DeleteOnTermination).To(BeTrue())
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.Encrypted).To(BeTrue())
				Expect(*ltInput.LaunchTemplateData.BlockDeviceMappings[0].Ebs.KmsKeyId).To(Equal("arn:aws:kms:us-west-2:111122223333:key/1234abcd-12ab-34cd-56ef-1234567890ab"))
			})
		})
	})
	Context("Ephemeral Storage", func() {
		It("should pack pods when a daemonset has an ephemeral-storage request", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClass, coretest.DaemonSet(
				coretest.DaemonSetOptions{PodOptions: coretest.PodOptions{
					ResourceRequirements: v1.ResourceRequirements{
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("1"),
							v1.ResourceMemory:           resource.MustParse("1Gi"),
							v1.ResourceEphemeralStorage: resource.MustParse("1Gi")}},
				}},
			))
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should pack pods with any ephemeral-storage request", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod(coretest.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceEphemeralStorage: resource.MustParse("1G"),
				}}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should pack pods with large ephemeral-storage request", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod(coretest.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
				}}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should not pack pods if the sum of pod ephemeral-storage and overhead exceeds node capacity", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod(coretest.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceEphemeralStorage: resource.MustParse("19Gi"),
				}}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should pack pods if the pod's ephemeral-storage exceeds node capacity and instance storage is mounted", func() {
			nodeClass.Spec.InstanceStorePolicy = lo.ToPtr(v1beta1.InstanceStorePolicyRAID0)
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod(coretest.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					// Default node ephemeral-storage capacity is 20Gi
					v1.ResourceEphemeralStorage: resource.MustParse("5000Gi"),
				}}})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			node := ExpectScheduled(ctx, env.Client, pod)
			Expect(node.Labels[v1.LabelInstanceTypeStable]).To(Equal("m6idn.32xlarge"))
			Expect(*node.Status.Capacity.StorageEphemeral()).To(Equal(resource.MustParse("7600G")))
		})
		It("should launch multiple nodes if sum of pod ephemeral-storage requests exceeds a single nodes capacity", func() {
			var nodes []*v1.Node
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pods := []*v1.Pod{
				coretest.UnschedulablePod(coretest.PodOptions{ResourceRequirements: v1.ResourceRequirements{
					Requests: map[v1.ResourceName]resource.Quantity{
						v1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
					},
				},
				}),
				coretest.UnschedulablePod(coretest.PodOptions{ResourceRequirements: v1.ResourceRequirements{
					Requests: map[v1.ResourceName]resource.Quantity{
						v1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
					},
				},
				}),
			}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
			for _, pod := range pods {
				nodes = append(nodes, ExpectScheduled(ctx, env.Client, pod))
			}
			Expect(nodes).To(HaveLen(2))
		})
		It("should only pack pods with ephemeral-storage requests that will fit on an available node", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pods := []*v1.Pod{
				coretest.UnschedulablePod(coretest.PodOptions{ResourceRequirements: v1.ResourceRequirements{
					Requests: map[v1.ResourceName]resource.Quantity{
						v1.ResourceEphemeralStorage: resource.MustParse("10Gi"),
					},
				},
				}),
				coretest.UnschedulablePod(coretest.PodOptions{ResourceRequirements: v1.ResourceRequirements{
					Requests: map[v1.ResourceName]resource.Quantity{
						v1.ResourceEphemeralStorage: resource.MustParse("150Gi"),
					},
				},
				}),
			}
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pods...)
			ExpectScheduled(ctx, env.Client, pods[0])
			ExpectNotScheduled(ctx, env.Client, pods[1])
		})
		It("should not pack pod if no available instance types have enough storage", func() {
			ExpectApplied(ctx, env.Client, nodePool)
			pod := coretest.UnschedulablePod(coretest.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceEphemeralStorage: resource.MustParse("150Gi"),
				},
			},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectNotScheduled(ctx, env.Client, pod)
		})
		It("should pack pods using the blockdevicemappings from the provider spec when defined", func() {
			nodeClass.Spec.BlockDeviceMappings = []*v1beta1.BlockDeviceMapping{
				{
					DeviceName: aws.String("/dev/xvda"),
					EBS: &v1beta1.BlockDevice{
						VolumeSize: resource.NewScaledQuantity(50, resource.Giga),
					},
				},
				{
					DeviceName: aws.String("/dev/xvdb"),
					EBS: &v1beta1.BlockDevice{
						VolumeSize: resource.NewScaledQuantity(20, resource.Giga),
					},
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod(coretest.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					v1.ResourceEphemeralStorage: resource.MustParse("25Gi"),
				},
			},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)

			// capacity isn't recorded on the node any longer, but we know the pod should schedule
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should pack pods using blockdevicemappings for Custom AMIFamily", func() {
			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyCustom
			nodeClass.Spec.AMISelectorTerms = []v1beta1.AMISelectorTerm{{Tags: map[string]string{"*": "*"}}}
			nodeClass.Spec.BlockDeviceMappings = []*v1beta1.BlockDeviceMapping{
				{
					DeviceName: aws.String("/dev/xvda"),
					EBS: &v1beta1.BlockDevice{
						VolumeSize: resource.NewScaledQuantity(20, resource.Giga),
					},
				},
				{
					DeviceName: aws.String("/dev/xvdb"),
					EBS: &v1beta1.BlockDevice{
						VolumeSize: resource.NewScaledQuantity(40, resource.Giga),
					},
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod(coretest.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					// this pod can only be satisfied if `/dev/xvdb` will house all the pods.
					v1.ResourceEphemeralStorage: resource.MustParse("25Gi"),
				},
			},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)

			// capacity isn't recorded on the node any longer, but we know the pod should schedule
			ExpectScheduled(ctx, env.Client, pod)
		})
		It("should pack pods using the configured root volume in blockdevicemappings", func() {
			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyCustom
			nodeClass.Spec.AMISelectorTerms = []v1beta1.AMISelectorTerm{{Tags: map[string]string{"*": "*"}}}
			nodeClass.Spec.BlockDeviceMappings = []*v1beta1.BlockDeviceMapping{
				{
					DeviceName: aws.String("/dev/xvda"),
					EBS: &v1beta1.BlockDevice{
						VolumeSize: resource.NewScaledQuantity(20, resource.Giga),
					},
				},
				{
					DeviceName: aws.String("/dev/xvdb"),
					EBS: &v1beta1.BlockDevice{
						VolumeSize: resource.NewScaledQuantity(40, resource.Giga),
					},
					RootVolume: true,
				},
				{
					DeviceName: aws.String("/dev/xvdc"),
					EBS: &v1beta1.BlockDevice{
						VolumeSize: resource.NewScaledQuantity(20, resource.Giga),
					},
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod(coretest.PodOptions{ResourceRequirements: v1.ResourceRequirements{
				Requests: map[v1.ResourceName]resource.Quantity{
					// this pod can only be satisfied if `/dev/xvdb` will house all the pods.
					v1.ResourceEphemeralStorage: resource.MustParse("25Gi"),
				},
			},
			})
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)

			// capacity isn't recorded on the node any longer, but we know the pod should schedule
			ExpectScheduled(ctx, env.Client, pod)
		})
	})
	Context("AL2", func() {
		var info *ec2.InstanceTypeInfo
		BeforeEach(func() {
			var ok bool
			var instanceInfo []*ec2.InstanceTypeInfo
			err := awsEnv.EC2API.DescribeInstanceTypesPagesWithContext(ctx, &ec2.DescribeInstanceTypesInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("supported-virtualization-type"),
						Values: []*string{aws.String("hvm")},
					},
					{
						Name:   aws.String("processor-info.supported-architecture"),
						Values: aws.StringSlice([]string{"x86_64", "arm64"}),
					},
				},
			}, func(page *ec2.DescribeInstanceTypesOutput, lastPage bool) bool {
				instanceInfo = append(instanceInfo, page.InstanceTypes...)
				return true
			})
			Expect(err).To(BeNil())
			info, ok = lo.Find(instanceInfo, func(i *ec2.InstanceTypeInfo) bool {
				return aws.StringValue(i.InstanceType) == "m5.xlarge"
			})
			Expect(ok).To(BeTrue())
		})

		It("should calculate memory overhead based on eni limited pods", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
				VMMemoryOverheadPercent: lo.ToPtr[float64](0),
			}))

			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyAL2
			amiFamily := amifamily.GetAMIFamily(nodeClass.Spec.AMIFamily, &amifamily.Options{})
			it := instancetype.NewInstanceType(ctx,
				info,
				"",
				nodeClass.Spec.BlockDeviceMappings,
				nodeClass.Spec.InstanceStorePolicy,
				nodePool.Spec.Template.Spec.Kubelet.MaxPods,
				nodePool.Spec.Template.Spec.Kubelet.PodsPerCore,
				nodePool.Spec.Template.Spec.Kubelet.KubeReserved,
				nodePool.Spec.Template.Spec.Kubelet.SystemReserved,
				nodePool.Spec.Template.Spec.Kubelet.EvictionHard,
				nodePool.Spec.Template.Spec.Kubelet.EvictionSoft,
				amiFamily,
				nil,
			)

			overhead := it.Overhead.Total()
			Expect(overhead.Memory().String()).To(Equal("993Mi"))
		})
	})
	Context("Bottlerocket", func() {
		var info *ec2.InstanceTypeInfo
		BeforeEach(func() {
			var ok bool
			var instanceInfo []*ec2.InstanceTypeInfo
			err := awsEnv.EC2API.DescribeInstanceTypesPagesWithContext(ctx, &ec2.DescribeInstanceTypesInput{
				Filters: []*ec2.Filter{
					{
						Name:   aws.String("supported-virtualization-type"),
						Values: []*string{aws.String("hvm")},
					},
					{
						Name:   aws.String("processor-info.supported-architecture"),
						Values: aws.StringSlice([]string{"x86_64", "arm64"}),
					},
				},
			}, func(page *ec2.DescribeInstanceTypesOutput, lastPage bool) bool {
				instanceInfo = append(instanceInfo, page.InstanceTypes...)
				return true
			})
			Expect(err).To(BeNil())
			info, ok = lo.Find(instanceInfo, func(i *ec2.InstanceTypeInfo) bool {
				return aws.StringValue(i.InstanceType) == "m5.xlarge"
			})
			Expect(ok).To(BeTrue())
		})

		It("should calculate memory overhead based on eni limited pods", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
				VMMemoryOverheadPercent: lo.ToPtr[float64](0),
			}))

			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyBottlerocket
			amiFamily := amifamily.GetAMIFamily(nodeClass.Spec.AMIFamily, &amifamily.Options{})
			it := instancetype.NewInstanceType(ctx,
				info,
				"",
				nodeClass.Spec.BlockDeviceMappings,
				nodeClass.Spec.InstanceStorePolicy,
				nodePool.Spec.Template.Spec.Kubelet.MaxPods,
				nodePool.Spec.Template.Spec.Kubelet.PodsPerCore,
				nodePool.Spec.Template.Spec.Kubelet.KubeReserved,
				nodePool.Spec.Template.Spec.Kubelet.SystemReserved,
				nodePool.Spec.Template.Spec.Kubelet.EvictionHard,
				nodePool.Spec.Template.Spec.Kubelet.EvictionSoft,
				amiFamily,
				nil,
			)

			overhead := it.Overhead.Total()
			Expect(overhead.Memory().String()).To(Equal("993Mi"))
		})
		It("should calculate memory overhead based on max pods", func() {
			ctx = options.ToContext(ctx, test.Options(test.OptionsFields{
				VMMemoryOverheadPercent: lo.ToPtr[float64](0),
			}))

			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyBottlerocket
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{MaxPods: lo.ToPtr[int32](110)}
			amiFamily := amifamily.GetAMIFamily(nodeClass.Spec.AMIFamily, &amifamily.Options{})
			it := instancetype.NewInstanceType(ctx,
				info,
				"",
				nodeClass.Spec.BlockDeviceMappings,
				nodeClass.Spec.InstanceStorePolicy,
				nodePool.Spec.Template.Spec.Kubelet.MaxPods,
				nodePool.Spec.Template.Spec.Kubelet.PodsPerCore,
				nodePool.Spec.Template.Spec.Kubelet.KubeReserved,
				nodePool.Spec.Template.Spec.Kubelet.SystemReserved,
				nodePool.Spec.Template.Spec.Kubelet.EvictionHard,
				nodePool.Spec.Template.Spec.Kubelet.EvictionSoft,
				amiFamily,
				nil,
			)
			overhead := it.Overhead.Total()
			Expect(overhead.Memory().String()).To(Equal("1565Mi"))
		})
	})
	Context("User Data", func() {
		It("should specify --use-max-pods=false when using ENI-based pod density", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			ExpectLaunchTemplatesCreatedWithUserDataContaining("--use-max-pods false")
		})
		It("should specify --use-max-pods=false and --max-pods user value when user specifies maxPods in NodePool", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{MaxPods: aws.Int32(10)}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			ExpectLaunchTemplatesCreatedWithUserDataContaining("--use-max-pods false", "--max-pods=10")
		})
		It("should specify --system-reserved when overriding system reserved values", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
				SystemReserved: map[string]string{
					string(v1.ResourceCPU):              "500m",
					string(v1.ResourceMemory):           "1Gi",
					string(v1.ResourceEphemeralStorage): "2Gi",
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
				Expect(err).To(BeNil())

				// Check whether the arguments are there for --system-reserved
				arg := "--system-reserved="
				i := strings.Index(string(userData), arg)
				rem := string(userData)[(i + len(arg)):]
				i = strings.Index(rem, "'")
				for k, v := range nodePool.Spec.Template.Spec.Kubelet.SystemReserved {
					Expect(rem[:i]).To(ContainSubstring(fmt.Sprintf("%v=%v", k, v)))
				}
			})
		})
		It("should specify --kube-reserved when overriding system reserved values", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
				KubeReserved: map[string]string{
					string(v1.ResourceCPU):              "500m",
					string(v1.ResourceMemory):           "1Gi",
					string(v1.ResourceEphemeralStorage): "2Gi",
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
				Expect(err).To(BeNil())

				// Check whether the arguments are there for --kube-reserved
				arg := "--kube-reserved="
				i := strings.Index(string(userData), arg)
				rem := string(userData)[(i + len(arg)):]
				i = strings.Index(rem, "'")
				for k, v := range nodePool.Spec.Template.Spec.Kubelet.KubeReserved {
					Expect(rem[:i]).To(ContainSubstring(fmt.Sprintf("%v=%v", k, v)))
				}
			})
		})
		It("should pass eviction hard threshold values when specified", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
				EvictionHard: map[string]string{
					"memory.available":  "10%",
					"nodefs.available":  "15%",
					"nodefs.inodesFree": "5%",
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
				Expect(err).To(BeNil())

				// Check whether the arguments are there for --kube-reserved
				arg := "--eviction-hard="
				i := strings.Index(string(userData), arg)
				rem := string(userData)[(i + len(arg)):]
				i = strings.Index(rem, "'")
				for k, v := range nodePool.Spec.Template.Spec.Kubelet.EvictionHard {
					Expect(rem[:i]).To(ContainSubstring(fmt.Sprintf("%v<%v", k, v)))
				}
			})
		})
		It("should pass eviction soft threshold values when specified", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
				EvictionSoft: map[string]string{
					"memory.available":  "10%",
					"nodefs.available":  "15%",
					"nodefs.inodesFree": "5%",
				},
				EvictionSoftGracePeriod: map[string]metav1.Duration{
					"memory.available":  {Duration: time.Minute},
					"nodefs.available":  {Duration: time.Second * 180},
					"nodefs.inodesFree": {Duration: time.Minute * 5},
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
				Expect(err).To(BeNil())

				// Check whether the arguments are there for --kube-reserved
				arg := "--eviction-soft="
				i := strings.Index(string(userData), arg)
				rem := string(userData)[(i + len(arg)):]
				i = strings.Index(rem, "'")
				for k, v := range nodePool.Spec.Template.Spec.Kubelet.EvictionSoft {
					Expect(rem[:i]).To(ContainSubstring(fmt.Sprintf("%v<%v", k, v)))
				}
			})
		})
		It("should pass eviction soft grace period values when specified", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
				EvictionSoftGracePeriod: map[string]metav1.Duration{
					"memory.available":  {Duration: time.Minute},
					"nodefs.available":  {Duration: time.Second * 180},
					"nodefs.inodesFree": {Duration: time.Minute * 5},
				},
				EvictionSoft: map[string]string{
					"memory.available":  "10%",
					"nodefs.available":  "15%",
					"nodefs.inodesFree": "5%",
				},
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
				Expect(err).To(BeNil())

				// Check whether the arguments are there for --kube-reserved
				arg := "--eviction-soft-grace-period="
				i := strings.Index(string(userData), arg)
				rem := string(userData)[(i + len(arg)):]
				i = strings.Index(rem, "'")
				for k, v := range nodePool.Spec.Template.Spec.Kubelet.EvictionSoftGracePeriod {
					Expect(rem[:i]).To(ContainSubstring(fmt.Sprintf("%v=%v", k, v.Duration.String())))
				}
			})
		})
		It("should pass eviction max pod grace period when specified", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
				EvictionMaxPodGracePeriod: aws.Int32(300),
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			ExpectLaunchTemplatesCreatedWithUserDataContaining(fmt.Sprintf("--eviction-max-pod-grace-period=%d", 300))
		})
		It("should specify --pods-per-core", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
				PodsPerCore: aws.Int32(2),
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			ExpectLaunchTemplatesCreatedWithUserDataContaining(fmt.Sprintf("--pods-per-core=%d", 2))
		})
		It("should specify --pods-per-core with --max-pods enabled", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
				PodsPerCore: aws.Int32(2),
				MaxPods:     aws.Int32(100),
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			ExpectLaunchTemplatesCreatedWithUserDataContaining(fmt.Sprintf("--pods-per-core=%d", 2), fmt.Sprintf("--max-pods=%d", 100))
		})
		It("should specify --dns-cluster-ip and --ip-family when running in an ipv6 cluster", func() {
			awsEnv.LaunchTemplateProvider.KubeDNSIP = net.ParseIP("fd4b:121b:812b::a")
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			ExpectLaunchTemplatesCreatedWithUserDataContaining("--dns-cluster-ip 'fd4b:121b:812b::a'")
			ExpectLaunchTemplatesCreatedWithUserDataContaining("--ip-family ipv6")
		})
		It("should specify --dns-cluster-ip when running in an ipv4 cluster", func() {
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			ExpectLaunchTemplatesCreatedWithUserDataContaining("--dns-cluster-ip '10.0.100.10'")
		})
		It("should pass ImageGCHighThresholdPercent when specified", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
				ImageGCHighThresholdPercent: aws.Int32(50),
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			ExpectLaunchTemplatesCreatedWithUserDataContaining("--image-gc-high-threshold=50")
		})
		It("should pass ImageGCLowThresholdPercent when specified", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
				ImageGCLowThresholdPercent: aws.Int32(50),
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			ExpectLaunchTemplatesCreatedWithUserDataContaining("--image-gc-low-threshold=50")
		})
		It("should pass --cpu-fs-quota when specified", func() {
			nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
				CPUCFSQuota: aws.Bool(false),
			}
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			ExpectLaunchTemplatesCreatedWithUserDataContaining("--cpu-cfs-quota=false")
		})
		It("should not pass any labels prefixed with the node-restriction.kubernetes.io domain", func() {
			nodePool.Spec.Template.Labels = lo.Assign(nodePool.Spec.Template.Labels, map[string]string{
				v1.LabelNamespaceNodeRestriction + "/team":                        "team-1",
				v1.LabelNamespaceNodeRestriction + "/custom-label":                "custom-value",
				"subdomain." + v1.LabelNamespaceNodeRestriction + "/custom-label": "custom-value",
			})
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			ExpectLaunchTemplatesCreatedWithUserDataNotContaining(v1.LabelNamespaceNodeRestriction)
		})
		It("should specify --local-disks raid0 when instance-store policy is set on AL2", func() {
			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyAL2
			nodeClass.Spec.InstanceStorePolicy = lo.ToPtr(v1beta1.InstanceStorePolicyRAID0)
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			ExpectLaunchTemplatesCreatedWithUserDataContaining("--local-disks raid0")
		})
		Context("Bottlerocket", func() {
			BeforeEach(func() {
				nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyBottlerocket
				nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{MaxPods: lo.ToPtr[int32](110)}
			})
			It("should merge in custom user data", func() {
				content, err := os.ReadFile("testdata/br_userdata_input.golden")
				Expect(err).To(BeNil())
				nodeClass.Spec.UserData = aws.String(fmt.Sprintf(string(content), corev1beta1.NodePoolLabelKey))
				nodePool.Spec.Template.Spec.Taints = []v1.Taint{{Key: "foo", Value: "bar", Effect: v1.TaintEffectNoExecute}}
				nodePool.Spec.Template.Spec.StartupTaints = []v1.Taint{{Key: "baz", Value: "bin", Effect: v1.TaintEffectNoExecute}}
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod(coretest.PodOptions{
					Tolerations: []v1.Toleration{{Operator: v1.TolerationOpExists}},
				})
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				content, err = os.ReadFile("testdata/br_userdata_merged.golden")
				Expect(err).To(BeNil())
				ExpectLaunchTemplatesCreatedWithUserData(fmt.Sprintf(string(content), corev1beta1.NodePoolLabelKey, nodePool.Name))
			})
			It("should bootstrap when custom user data is empty", func() {
				nodePool.Spec.Template.Spec.Taints = []v1.Taint{{Key: "foo", Value: "bar", Effect: v1.TaintEffectNoExecute}}
				nodePool.Spec.Template.Spec.StartupTaints = []v1.Taint{{Key: "baz", Value: "bin", Effect: v1.TaintEffectNoExecute}}
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(nodePool), nodePool)).To(Succeed())
				pod := coretest.UnschedulablePod(coretest.PodOptions{
					Tolerations: []v1.Toleration{{Operator: v1.TolerationOpExists}},
				})
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				content, err := os.ReadFile("testdata/br_userdata_unmerged.golden")
				Expect(err).To(BeNil())
				ExpectLaunchTemplatesCreatedWithUserData(fmt.Sprintf(string(content), corev1beta1.NodePoolLabelKey, nodePool.Name))
			})
			It("should not bootstrap when provider ref points to a non-existent EC2NodeClass resource", func() {
				nodePool.Spec.Template.Spec.NodeClassRef = &corev1beta1.NodeClassReference{Name: "doesnotexist"}
				ExpectApplied(ctx, env.Client, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				// This will not be scheduled since we were pointed to a non-existent EC2NodeClass resource.
				ExpectNotScheduled(ctx, env.Client, pod)
			})
			It("should not bootstrap on invalid toml user data", func() {
				nodeClass.Spec.UserData = aws.String("#/bin/bash\n ./not-toml.sh")
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				// This will not be scheduled since userData cannot be generated for the prospective node.
				ExpectNotScheduled(ctx, env.Client, pod)
			})
			It("should override system reserved values in user data", func() {
				ExpectApplied(ctx, env.Client, nodeClass)
				nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
					SystemReserved: map[string]string{
						string(v1.ResourceCPU):              "2",
						string(v1.ResourceMemory):           "3Gi",
						string(v1.ResourceEphemeralStorage): "10Gi",
					},
				}
				ExpectApplied(ctx, env.Client, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
				awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
					userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
					Expect(err).To(BeNil())
					config := &bootstrap.BottlerocketConfig{}
					Expect(config.UnmarshalTOML(userData)).To(Succeed())
					Expect(len(config.Settings.Kubernetes.SystemReserved)).To(Equal(3))
					Expect(config.Settings.Kubernetes.SystemReserved[v1.ResourceCPU.String()]).To(Equal("2"))
					Expect(config.Settings.Kubernetes.SystemReserved[v1.ResourceMemory.String()]).To(Equal("3Gi"))
					Expect(config.Settings.Kubernetes.SystemReserved[v1.ResourceEphemeralStorage.String()]).To(Equal("10Gi"))
				})
			})
			It("should override kube reserved values in user data", func() {
				ExpectApplied(ctx, env.Client, nodeClass)
				nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
					KubeReserved: map[string]string{
						string(v1.ResourceCPU):              "2",
						string(v1.ResourceMemory):           "3Gi",
						string(v1.ResourceEphemeralStorage): "10Gi",
					},
				}
				ExpectApplied(ctx, env.Client, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
				awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
					userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
					Expect(err).To(BeNil())
					config := &bootstrap.BottlerocketConfig{}
					Expect(config.UnmarshalTOML(userData)).To(Succeed())
					Expect(len(config.Settings.Kubernetes.KubeReserved)).To(Equal(3))
					Expect(config.Settings.Kubernetes.KubeReserved[v1.ResourceCPU.String()]).To(Equal("2"))
					Expect(config.Settings.Kubernetes.KubeReserved[v1.ResourceMemory.String()]).To(Equal("3Gi"))
					Expect(config.Settings.Kubernetes.KubeReserved[v1.ResourceEphemeralStorage.String()]).To(Equal("10Gi"))
				})
			})
			It("should override kube reserved values in user data", func() {
				ExpectApplied(ctx, env.Client, nodeClass)
				nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
					EvictionHard: map[string]string{
						"memory.available":  "10%",
						"nodefs.available":  "15%",
						"nodefs.inodesFree": "5%",
					},
				}
				ExpectApplied(ctx, env.Client, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
				awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
					userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
					Expect(err).To(BeNil())
					config := &bootstrap.BottlerocketConfig{}
					Expect(config.UnmarshalTOML(userData)).To(Succeed())
					Expect(len(config.Settings.Kubernetes.EvictionHard)).To(Equal(3))
					Expect(config.Settings.Kubernetes.EvictionHard["memory.available"]).To(Equal("10%"))
					Expect(config.Settings.Kubernetes.EvictionHard["nodefs.available"]).To(Equal("15%"))
					Expect(config.Settings.Kubernetes.EvictionHard["nodefs.inodesFree"]).To(Equal("5%"))
				})
			})
			It("should specify max pods value when passing maxPods in configuration", func() {
				nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
					MaxPods: aws.Int32(10),
				}
				ExpectApplied(ctx, env.Client, nodePool, nodeClass)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
				awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
					userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
					Expect(err).To(BeNil())
					config := &bootstrap.BottlerocketConfig{}
					Expect(config.UnmarshalTOML(userData)).To(Succeed())
					Expect(config.Settings.Kubernetes.MaxPods).ToNot(BeNil())
					Expect(*config.Settings.Kubernetes.MaxPods).To(BeNumerically("==", 10))
				})
			})
			It("should pass ImageGCHighThresholdPercent when specified", func() {
				nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
					ImageGCHighThresholdPercent: aws.Int32(50),
				}
				ExpectApplied(ctx, env.Client, nodePool, nodeClass)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
				awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
					userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
					Expect(err).To(BeNil())
					config := &bootstrap.BottlerocketConfig{}
					Expect(config.UnmarshalTOML(userData)).To(Succeed())
					Expect(config.Settings.Kubernetes.ImageGCHighThresholdPercent).ToNot(BeNil())
					percent, err := strconv.Atoi(*config.Settings.Kubernetes.ImageGCHighThresholdPercent)
					Expect(err).ToNot(HaveOccurred())
					Expect(percent).To(BeNumerically("==", 50))
				})
			})
			It("should pass ImageGCLowThresholdPercent when specified", func() {
				nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
					ImageGCLowThresholdPercent: aws.Int32(50),
				}
				ExpectApplied(ctx, env.Client, nodePool, nodeClass)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
				awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
					userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
					Expect(err).To(BeNil())
					config := &bootstrap.BottlerocketConfig{}
					Expect(config.UnmarshalTOML(userData)).To(Succeed())
					Expect(config.Settings.Kubernetes.ImageGCLowThresholdPercent).ToNot(BeNil())
					percent, err := strconv.Atoi(*config.Settings.Kubernetes.ImageGCLowThresholdPercent)
					Expect(err).ToNot(HaveOccurred())
					Expect(percent).To(BeNumerically("==", 50))
				})
			})
			It("should pass ClusterDNSIP when discovered", func() {
				ExpectApplied(ctx, env.Client, nodePool, nodeClass)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
				awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
					userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
					Expect(err).To(BeNil())
					config := &bootstrap.BottlerocketConfig{}
					Expect(config.UnmarshalTOML(userData)).To(Succeed())
					Expect(config.Settings.Kubernetes.ClusterDNSIP).ToNot(BeNil())
					Expect(*config.Settings.Kubernetes.ClusterDNSIP).To(Equal("10.0.100.10"))
				})
			})
			It("should pass CPUCFSQuota when specified", func() {
				nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{
					CPUCFSQuota: aws.Bool(false),
				}
				ExpectApplied(ctx, env.Client, nodePool, nodeClass)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
				awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
					userData, err := base64.StdEncoding.DecodeString(*ltInput.LaunchTemplateData.UserData)
					Expect(err).To(BeNil())
					config := &bootstrap.BottlerocketConfig{}
					Expect(config.UnmarshalTOML(userData)).To(Succeed())
					Expect(config.Settings.Kubernetes.CPUCFSQuota).ToNot(BeNil())
					Expect(*config.Settings.Kubernetes.CPUCFSQuota).To(BeFalse())
				})
			})
		})
		Context("AL2 Custom UserData", func() {
			BeforeEach(func() {
				nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{MaxPods: lo.ToPtr[int32](110)}
			})
			It("should merge in custom user data", func() {
				content, err := os.ReadFile("testdata/al2_userdata_input.golden")
				Expect(err).To(BeNil())
				nodeClass.Spec.UserData = aws.String(string(content))
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				content, err = os.ReadFile("testdata/al2_userdata_merged.golden")
				Expect(err).To(BeNil())
				expectedUserData := fmt.Sprintf(string(content), corev1beta1.NodePoolLabelKey, nodePool.Name)
				ExpectLaunchTemplatesCreatedWithUserData(expectedUserData)
			})
			It("should merge in custom user data when Content-Type is before MIME-Version", func() {
				content, err := os.ReadFile("testdata/al2_userdata_content_type_first_input.golden")
				Expect(err).To(BeNil())
				nodeClass.Spec.UserData = aws.String(string(content))
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				content, err = os.ReadFile("testdata/al2_userdata_merged.golden")
				Expect(err).To(BeNil())
				expectedUserData := fmt.Sprintf(string(content), corev1beta1.NodePoolLabelKey, nodePool.Name)
				ExpectLaunchTemplatesCreatedWithUserData(expectedUserData)
			})
			It("should merge in custom user data not in multi-part mime format", func() {
				content, err := os.ReadFile("testdata/al2_no_mime_userdata_input.golden")
				Expect(err).To(BeNil())
				nodeClass.Spec.UserData = aws.String(string(content))
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				content, err = os.ReadFile("testdata/al2_userdata_merged.golden")
				Expect(err).To(BeNil())
				expectedUserData := fmt.Sprintf(string(content), corev1beta1.NodePoolLabelKey, nodePool.Name)
				ExpectLaunchTemplatesCreatedWithUserData(expectedUserData)
			})
			It("should handle empty custom user data", func() {
				nodeClass.Spec.UserData = nil
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				content, err := os.ReadFile("testdata/al2_userdata_unmerged.golden")
				Expect(err).To(BeNil())
				expectedUserData := fmt.Sprintf(string(content), corev1beta1.NodePoolLabelKey, nodePool.Name)
				ExpectLaunchTemplatesCreatedWithUserData(expectedUserData)
			})
		})
		Context("AL2023", func() {
			BeforeEach(func() {
				nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyAL2023

				// base64 encoded version of "ca-bundle" to ensure the nodeadm bootstrap provider can decode successfully
				awsEnv.LaunchTemplateProvider.CABundle = lo.ToPtr("Y2EtYnVuZGxlCg==")
				awsEnv.LaunchTemplateProvider.ClusterCIDR.Store(lo.ToPtr("10.100.0.0/16"))
			})
			Context("Kubelet", func() {
				It("should specify taints in the KubeletConfiguration when specified in NodePool", func() {
					desiredTaints := []v1.Taint{
						{
							Key:    "test-taint-1",
							Effect: v1.TaintEffectNoSchedule,
						},
						{
							Key:    "test-taint-2",
							Effect: v1.TaintEffectNoExecute,
						},
					}
					nodePool.Spec.Template.Spec.Taints = desiredTaints
					ExpectApplied(ctx, env.Client, nodePool, nodeClass)
					pod := coretest.UnschedulablePod(coretest.UnscheduleablePodOptions(coretest.PodOptions{
						Tolerations: []v1.Toleration{{
							Operator: v1.TolerationOpExists,
						}},
					}))
					ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
					ExpectScheduled(ctx, env.Client, pod)
					for _, userData := range ExpectUserDataExistsFromCreatedLaunchTemplates() {
						configs := ExpectUserDataCreatedWithNodeConfigs(userData)
						Expect(len(configs)).To(Equal(1))
						taintsRaw, ok := configs[0].Spec.Kubelet.Config["registerWithTaints"]
						Expect(ok).To(BeTrue())
						taints := []v1.Taint{}
						Expect(yaml.Unmarshal(taintsRaw.Raw, &taints)).To(Succeed())
						Expect(len(taints)).To(Equal(2))
						Expect(taints).To(ContainElements(lo.Map(desiredTaints, func(t v1.Taint, _ int) interface{} {
							return interface{}(t)
						})))
					}
				})
				It("should specify labels in the Kubelet flags when specified in NodePool", func() {
					desiredLabels := map[string]string{
						"test-label-1": "value-1",
						"test-label-2": "value-2",
					}
					nodePool.Spec.Template.Labels = desiredLabels

					ExpectApplied(ctx, env.Client, nodePool, nodeClass)
					pod := coretest.UnschedulablePod()
					ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
					ExpectScheduled(ctx, env.Client, pod)
					for _, userData := range ExpectUserDataExistsFromCreatedLaunchTemplates() {
						configs := ExpectUserDataCreatedWithNodeConfigs(userData)
						Expect(len(configs)).To(Equal(1))
						labelFlag, ok := lo.Find(configs[0].Spec.Kubelet.Flags, func(flag string) bool {
							return strings.HasPrefix(flag, "--node-labels")
						})
						Expect(ok).To(BeTrue())
						for label, value := range desiredLabels {
							Expect(labelFlag).To(ContainSubstring(fmt.Sprintf("%s=%s", label, value)))
						}
					}
				})
				DescribeTable(
					"should specify KubletConfiguration field when specified in NodePool",
					func(field string, kc corev1beta1.KubeletConfiguration) {
						nodePool.Spec.Template.Spec.Kubelet = &kc
						ExpectApplied(ctx, env.Client, nodePool, nodeClass)
						pod := coretest.UnschedulablePod()
						ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
						ExpectScheduled(ctx, env.Client, pod)

						// Convert provided KubeletConfiguration to an InlineConfig for comparison with NodeConfig
						inlineConfig := func() map[string]runtime.RawExtension {
							configYAML, err := yaml.Marshal(kc)
							Expect(err).To(BeNil())
							configMap := map[string]interface{}{}
							Expect(yaml.Unmarshal(configYAML, &configMap)).To(Succeed())
							return lo.MapValues(configMap, func(v interface{}, _ string) runtime.RawExtension {
								val, err := json.Marshal(v)
								Expect(err).To(BeNil())
								return runtime.RawExtension{Raw: val}
							})
						}()
						for _, userData := range ExpectUserDataExistsFromCreatedLaunchTemplates() {
							configs := ExpectUserDataCreatedWithNodeConfigs(userData)
							Expect(len(configs)).To(Equal(1))
							Expect(configs[0].Spec.Kubelet.Config[field]).To(Equal(inlineConfig[field]))
						}
					},
					Entry("systemReserved", "systemReserved", corev1beta1.KubeletConfiguration{
						SystemReserved: map[string]string{
							string(v1.ResourceCPU):              "500m",
							string(v1.ResourceMemory):           "1Gi",
							string(v1.ResourceEphemeralStorage): "2Gi",
						},
					}),
					Entry("kubeReserved", "kubeReserved", corev1beta1.KubeletConfiguration{
						KubeReserved: map[string]string{
							string(v1.ResourceCPU):              "500m",
							string(v1.ResourceMemory):           "1Gi",
							string(v1.ResourceEphemeralStorage): "2Gi",
						},
					}),
					Entry("evictionHard", "evictionHard", corev1beta1.KubeletConfiguration{
						EvictionHard: map[string]string{
							"memory.available":  "10%",
							"nodefs.available":  "15%",
							"nodefs.inodesFree": "5%",
						},
					}),
					Entry("evictionSoft", "evictionSoft", corev1beta1.KubeletConfiguration{
						EvictionSoft: map[string]string{
							"memory.available":  "10%",
							"nodefs.available":  "15%",
							"nodefs.inodesFree": "5%",
						},
						EvictionSoftGracePeriod: map[string]metav1.Duration{
							"memory.available":  {Duration: time.Minute},
							"nodefs.available":  {Duration: time.Second * 180},
							"nodefs.inodesFree": {Duration: time.Minute * 5},
						},
					}),
					Entry("evictionSoftGracePeriod", "evictionSoftGracePeriod", corev1beta1.KubeletConfiguration{
						EvictionSoft: map[string]string{
							"memory.available":  "10%",
							"nodefs.available":  "15%",
							"nodefs.inodesFree": "5%",
						},
						EvictionSoftGracePeriod: map[string]metav1.Duration{
							"memory.available":  {Duration: time.Minute},
							"nodefs.available":  {Duration: time.Second * 180},
							"nodefs.inodesFree": {Duration: time.Minute * 5},
						},
					}),
					Entry("evictionMaxPodGracePeriod", "evictionMaxPodGracePeriod", corev1beta1.KubeletConfiguration{
						EvictionMaxPodGracePeriod: lo.ToPtr[int32](300),
					}),
					Entry("podsPerCore", "podsPerCore", corev1beta1.KubeletConfiguration{
						PodsPerCore: lo.ToPtr[int32](2),
					}),
					Entry("clusterDNS", "clusterDNS", corev1beta1.KubeletConfiguration{
						ClusterDNS: []string{"10.0.100.0"},
					}),
					Entry("imageGCHighThresholdPercent", "imageGCHighThresholdPercent", corev1beta1.KubeletConfiguration{
						ImageGCHighThresholdPercent: lo.ToPtr[int32](50),
					}),
					Entry("imageGCLowThresholdPercent", "imageGCLowThresholdPercent", corev1beta1.KubeletConfiguration{
						ImageGCLowThresholdPercent: lo.ToPtr[int32](50),
					}),
					Entry("cpuCFSQuota", "cpuCFSQuota", corev1beta1.KubeletConfiguration{
						CPUCFSQuota: lo.ToPtr(false),
					}),
				)
			})
			It("should set LocalDiskStrategy to Raid0 when specified by the InstanceStorePolicy", func() {
				nodeClass.Spec.InstanceStorePolicy = lo.ToPtr(v1beta1.InstanceStorePolicyRAID0)
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				for _, userData := range ExpectUserDataExistsFromCreatedLaunchTemplates() {
					configs := ExpectUserDataCreatedWithNodeConfigs(userData)
					Expect(len(configs)).To(Equal(1))
					Expect(configs[0].Spec.Instance.LocalStorage.Strategy).To(Equal(admv1alpha1.LocalStorageRAID0))
				}
			})
			DescribeTable(
				"should merge custom user data",
				func(inputFile *string, mergedFile string) {
					if inputFile != nil {
						content, err := os.ReadFile("testdata/" + *inputFile)
						Expect(err).To(BeNil())
						nodeClass.Spec.UserData = lo.ToPtr(string(content))
					}
					nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{MaxPods: lo.ToPtr[int32](110)}
					ExpectApplied(ctx, env.Client, nodeClass, nodePool)
					pod := coretest.UnschedulablePod()
					ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
					ExpectScheduled(ctx, env.Client, pod)
					content, err := os.ReadFile("testdata/" + mergedFile)
					Expect(err).To(BeNil())
					expectedUserData := fmt.Sprintf(string(content), corev1beta1.NodePoolLabelKey, nodePool.Name)
					ExpectLaunchTemplatesCreatedWithUserData(expectedUserData)
				},
				Entry("MIME", lo.ToPtr("al2023_mime_userdata_input.golden"), "al2023_mime_userdata_merged.golden"),
				Entry("YAML", lo.ToPtr("al2023_yaml_userdata_input.golden"), "al2023_yaml_userdata_merged.golden"),
				Entry("shell", lo.ToPtr("al2023_shell_userdata_input.golden"), "al2023_shell_userdata_merged.golden"),
				Entry("empty", nil, "al2023_userdata_unmerged.golden"),
			)
			It("should fail to create launch templates if cluster CIDR is unresolved", func() {
				awsEnv.LaunchTemplateProvider.ClusterCIDR.Store(nil)
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectNotScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(Equal(0))
			})
		})
		Context("Custom AMI Selector", func() {
			It("should use ami selector specified in EC2NodeClass", func() {
				nodeClass.Spec.AMISelectorTerms = []v1beta1.AMISelectorTerm{{Tags: map[string]string{"*": "*"}}}
				nodeClass.Status.AMIs = []v1beta1.AMI{
					{
						ID: "ami-123",
						Requirements: []v1.NodeSelectorRequirement{
							{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{corev1beta1.ArchitectureAmd64}},
						},
					},
				}
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
				awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
					Expect("ami-123").To(Equal(*ltInput.LaunchTemplateData.ImageId))
				})
			})
			It("should copy over userData untouched when AMIFamily is Custom", func() {
				nodeClass.Spec.UserData = aws.String("special user data")
				nodeClass.Spec.AMISelectorTerms = []v1beta1.AMISelectorTerm{{Tags: map[string]string{"*": "*"}}}
				nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyCustom
				nodeClass.Status.AMIs = []v1beta1.AMI{
					{
						ID: "ami-123",
						Requirements: []v1.NodeSelectorRequirement{
							{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{corev1beta1.ArchitectureAmd64}},
						},
					},
				}
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				ExpectLaunchTemplatesCreatedWithUserData("special user data")
			})
			It("should correctly use ami selector with specific IDs in EC2NodeClass", func() {
				nodeClass.Spec.AMISelectorTerms = []v1beta1.AMISelectorTerm{{ID: "ami-123"}, {ID: "ami-456"}}
				awsEnv.EC2API.DescribeImagesOutput.Set(&ec2.DescribeImagesOutput{Images: []*ec2.Image{
					{
						Name:         aws.String(coretest.RandomName()),
						ImageId:      aws.String("ami-123"),
						Architecture: aws.String("x86_64"),
						Tags:         []*ec2.Tag{{Key: aws.String(v1.LabelInstanceTypeStable), Value: aws.String("m5.large")}},
						CreationDate: aws.String("2022-08-15T12:00:00Z"),
					},
					{
						Name:         aws.String(coretest.RandomName()),
						ImageId:      aws.String("ami-456"),
						Architecture: aws.String("x86_64"),
						Tags:         []*ec2.Tag{{Key: aws.String(v1.LabelInstanceTypeStable), Value: aws.String("m5.xlarge")}},
						CreationDate: aws.String("2022-08-15T12:00:00Z"),
					},
				}})
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				_, err := awsEnv.AMIProvider.List(ctx, nodeClass)
				Expect(err).To(BeNil())
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 2))
				actualFilter := awsEnv.EC2API.CalledWithDescribeImagesInput.Pop().Filters
				expectedFilter := []*ec2.Filter{
					{
						Name:   aws.String("image-id"),
						Values: aws.StringSlice([]string{"ami-123", "ami-456"}),
					},
				}
				Expect(actualFilter).To(Equal(expectedFilter))
			})
			It("should create multiple launch templates when multiple amis are discovered with non-equivalent requirements", func() {
				nodeClass.Spec.AMISelectorTerms = []v1beta1.AMISelectorTerm{{Tags: map[string]string{"*": "*"}}}
				nodeClass.Status.AMIs = []v1beta1.AMI{
					{
						ID: "ami-123",
						Requirements: []v1.NodeSelectorRequirement{
							{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{corev1beta1.ArchitectureAmd64}},
						},
					},
					{
						ID: "ami-456",
						Requirements: []v1.NodeSelectorRequirement{
							{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{corev1beta1.ArchitectureArm64}},
						},
					},
				}
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 2))
				expectedImageIds := sets.New[string]("ami-123", "ami-456")
				actualImageIds := sets.New[string]()
				awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
					actualImageIds.Insert(*ltInput.LaunchTemplateData.ImageId)
				})
				Expect(expectedImageIds.Equal(actualImageIds)).To(BeTrue())
			})
			It("should create a launch template with the newest compatible AMI when multiple amis are discovered", func() {
				awsEnv.EC2API.DescribeImagesOutput.Set(&ec2.DescribeImagesOutput{Images: []*ec2.Image{
					{
						Name:         aws.String(coretest.RandomName()),
						ImageId:      aws.String("ami-123"),
						Architecture: aws.String("x86_64"),
						CreationDate: aws.String("2020-01-01T12:00:00Z"),
					},
					{
						Name:         aws.String(coretest.RandomName()),
						ImageId:      aws.String("ami-456"),
						Architecture: aws.String("x86_64"),
						CreationDate: aws.String("2021-01-01T12:00:00Z"),
					},
					{
						// Incompatible because required ARM64
						Name:         aws.String(coretest.RandomName()),
						ImageId:      aws.String("ami-789"),
						Architecture: aws.String("arm64"),
						CreationDate: aws.String("2022-01-01T12:00:00Z"),
					},
				}})
				nodeClass.Spec.AMISelectorTerms = []v1beta1.AMISelectorTerm{{Tags: map[string]string{"*": "*"}}}
				ExpectApplied(ctx, env.Client, nodeClass)
				controller := status.NewController(env.Client, awsEnv.SubnetProvider, awsEnv.SecurityGroupProvider, awsEnv.AMIProvider, awsEnv.InstanceProfileProvider, awsEnv.LaunchTemplateProvider)
				ExpectObjectReconciled(ctx, env.Client, controller, nodeClass)
				nodePool.Spec.Template.Spec.Requirements = []corev1beta1.NodeSelectorRequirementWithMinValues{
					{
						NodeSelectorRequirement: v1.NodeSelectorRequirement{
							Key:      v1.LabelArchStable,
							Operator: v1.NodeSelectorOpIn,
							Values:   []string{corev1beta1.ArchitectureAmd64},
						},
					},
				}
				ExpectApplied(ctx, env.Client, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
				awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
					Expect("ami-456").To(Equal(*ltInput.LaunchTemplateData.ImageId))
				})
			})

			It("should fail if no amis match selector.", func() {
				awsEnv.EC2API.DescribeImagesOutput.Set(&ec2.DescribeImagesOutput{Images: []*ec2.Image{}})
				nodeClass.Spec.AMISelectorTerms = []v1beta1.AMISelectorTerm{{Tags: map[string]string{"*": "*"}}}
				nodeClass.Status.AMIs = []v1beta1.AMI{}
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectNotScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(Equal(0))
			})
			It("should fail if no instanceType matches ami requirements.", func() {
				awsEnv.EC2API.DescribeImagesOutput.Set(&ec2.DescribeImagesOutput{Images: []*ec2.Image{
					{Name: aws.String(coretest.RandomName()), ImageId: aws.String("ami-123"), Architecture: aws.String("newnew"), CreationDate: aws.String("2022-01-01T12:00:00Z")}}})
				nodeClass.Spec.AMISelectorTerms = []v1beta1.AMISelectorTerm{{Tags: map[string]string{"*": "*"}}}
				nodeClass.Status.AMIs = []v1beta1.AMI{
					{
						ID: "ami-123",
						Requirements: []v1.NodeSelectorRequirement{
							{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{"newnew"}},
						},
					},
				}
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectNotScheduled(ctx, env.Client, pod)
				Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(Equal(0))
			})
			It("should choose amis from SSM if no selector specified in EC2NodeClass", func() {
				version := lo.Must(awsEnv.VersionProvider.Get(ctx))
				awsEnv.SSMAPI.Parameters = map[string]string{
					fmt.Sprintf("/aws/service/eks/optimized-ami/%s/amazon-linux-2/recommended/image_id", version): "test-ami-123",
				}
				nodeClass.Status.AMIs = []v1beta1.AMI{
					{
						ID: "test-ami-123",
						Requirements: []v1.NodeSelectorRequirement{
							{Key: v1.LabelArchStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(corev1beta1.ArchitectureAmd64)}},
						},
					},
				}
				ExpectApplied(ctx, env.Client, nodeClass)
				ExpectApplied(ctx, env.Client, nodePool)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				input := awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Pop()
				Expect(*input.LaunchTemplateData.ImageId).To(ContainSubstring("test-ami"))
			})
		})
		Context("Public IP Association", func() {
			It("should explicitly set 'AssociatePublicIPAddress' to false in the Launch Template", func() {
				nodeClass.Spec.SubnetSelectorTerms = []v1beta1.SubnetSelectorTerm{
					{Tags: map[string]string{"Name": "test-subnet-1"}},
					{Tags: map[string]string{"Name": "test-subnet-3"}},
				}
				ExpectApplied(ctx, env.Client, nodePool, nodeClass)
				controller := status.NewController(env.Client, awsEnv.SubnetProvider, awsEnv.SecurityGroupProvider, awsEnv.AMIProvider, awsEnv.InstanceProfileProvider, awsEnv.LaunchTemplateProvider)
				ExpectObjectReconciled(ctx, env.Client, controller, nodeClass)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				input := awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Pop()
				Expect(*input.LaunchTemplateData.NetworkInterfaces[0].AssociatePublicIpAddress).To(BeFalse())
			})
			It("should not explicitly set 'AssociatePublicIPAddress' when the subnets are configured to assign public IPv4 addresses", func() {
				nodeClass.Spec.SubnetSelectorTerms = []v1beta1.SubnetSelectorTerm{
					{Tags: map[string]string{"Name": "test-subnet-2"}},
				}
				ExpectApplied(ctx, env.Client, nodePool, nodeClass)
				controller := status.NewController(env.Client, awsEnv.SubnetProvider, awsEnv.SecurityGroupProvider, awsEnv.AMIProvider, awsEnv.InstanceProfileProvider, awsEnv.LaunchTemplateProvider)
				ExpectObjectReconciled(ctx, env.Client, controller, nodeClass)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				input := awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Pop()
				Expect(len(input.LaunchTemplateData.NetworkInterfaces)).To(BeNumerically("==", 0))
			})
			DescribeTable(
				"should set 'AssociatePublicIPAddress' based on EC2NodeClass",
				func(setValue, expectedValue, isEFA bool) {
					nodeClass.Spec.AssociatePublicIPAddress = lo.ToPtr(setValue)
					ExpectApplied(ctx, env.Client, nodePool, nodeClass)
					pod := coretest.UnschedulablePod(lo.Ternary(isEFA, coretest.PodOptions{
						ResourceRequirements: v1.ResourceRequirements{
							Requests: v1.ResourceList{v1beta1.ResourceEFA: resource.MustParse("2")},
							Limits:   v1.ResourceList{v1beta1.ResourceEFA: resource.MustParse("2")},
						},
					}, coretest.PodOptions{}))
					ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
					ExpectScheduled(ctx, env.Client, pod)
					input := awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Pop()
					Expect(*input.LaunchTemplateData.NetworkInterfaces[0].AssociatePublicIpAddress).To(Equal(expectedValue))
				},
				Entry("AssociatePublicIPAddress is true", true, true, false),
				Entry("AssociatePublicIPAddress is false", false, false, false),
				Entry("AssociatePublicIPAddress is true (EFA)", true, true, true),
				Entry("AssociatePublicIPAddress is false (EFA)", false, false, true),
			)
		})
		Context("Kubelet Args", func() {
			It("should specify the --dns-cluster-ip flag when clusterDNSIP is set", func() {
				nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{ClusterDNS: []string{"10.0.10.100"}}
				ExpectApplied(ctx, env.Client, nodePool, nodeClass)
				pod := coretest.UnschedulablePod()
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				ExpectLaunchTemplatesCreatedWithUserDataContaining("--dns-cluster-ip '10.0.10.100'")
			})
		})
		Context("Windows Custom UserData", func() {
			BeforeEach(func() {
				nodePool.Spec.Template.Spec.Requirements = []corev1beta1.NodeSelectorRequirementWithMinValues{{NodeSelectorRequirement: v1.NodeSelectorRequirement{Key: v1.LabelOSStable, Operator: v1.NodeSelectorOpIn, Values: []string{string(v1.Windows)}}}}
				nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyWindows2022
				nodePool.Spec.Template.Spec.Kubelet = &corev1beta1.KubeletConfiguration{MaxPods: lo.ToPtr[int32](110)}
			})
			It("should merge and bootstrap with custom user data", func() {
				content, err := os.ReadFile("testdata/windows_userdata_input.golden")
				Expect(err).To(BeNil())
				nodeClass.Spec.UserData = aws.String(string(content))
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(nodePool), nodePool)).To(Succeed())
				pod := coretest.UnschedulablePod(coretest.PodOptions{
					NodeSelector: map[string]string{
						v1.LabelOSStable:     string(v1.Windows),
						v1.LabelWindowsBuild: "10.0.20348",
					},
				})
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				content, err = os.ReadFile("testdata/windows_userdata_merged.golden")
				Expect(err).To(BeNil())
				ExpectLaunchTemplatesCreatedWithUserData(fmt.Sprintf(string(content), corev1beta1.NodePoolLabelKey, nodePool.Name))
			})
			It("should bootstrap when custom user data is empty", func() {
				ExpectApplied(ctx, env.Client, nodeClass, nodePool)
				Expect(env.Client.Get(ctx, client.ObjectKeyFromObject(nodePool), nodePool)).To(Succeed())
				pod := coretest.UnschedulablePod(coretest.PodOptions{
					NodeSelector: map[string]string{
						v1.LabelOSStable:     string(v1.Windows),
						v1.LabelWindowsBuild: "10.0.20348",
					},
				})
				ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
				ExpectScheduled(ctx, env.Client, pod)
				content, err := os.ReadFile("testdata/windows_userdata_unmerged.golden")
				Expect(err).To(BeNil())
				ExpectLaunchTemplatesCreatedWithUserData(fmt.Sprintf(string(content), corev1beta1.NodePoolLabelKey, nodePool.Name))
			})
		})
	})
	Context("Detailed Monitoring", func() {
		It("should default detailed monitoring to off", func() {
			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyAL2
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				Expect(aws.BoolValue(ltInput.LaunchTemplateData.Monitoring.Enabled)).To(BeFalse())
			})
		})
		It("should pass detailed monitoring setting to the launch template at creation", func() {
			nodeClass.Spec.AMIFamily = &v1beta1.AMIFamilyAL2
			nodeClass.Spec.DetailedMonitoring = aws.Bool(true)
			ExpectApplied(ctx, env.Client, nodePool, nodeClass)
			pod := coretest.UnschedulablePod()
			ExpectProvisioned(ctx, env.Client, cluster, cloudProvider, prov, pod)
			ExpectScheduled(ctx, env.Client, pod)
			Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
			awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(ltInput *ec2.CreateLaunchTemplateInput) {
				Expect(aws.BoolValue(ltInput.LaunchTemplateData.Monitoring.Enabled)).To(BeTrue())
			})
		})
	})
})

// ExpectTags verifies that the expected tags are a subset of the tags found
func ExpectTags(tags []*ec2.Tag, expected map[string]string) {
	GinkgoHelper()
	existingTags := lo.SliceToMap(tags, func(t *ec2.Tag) (string, string) { return *t.Key, *t.Value })
	for expKey, expValue := range expected {
		foundValue, ok := existingTags[expKey]
		Expect(ok).To(BeTrue(), fmt.Sprintf("expected to find tag %s in %s", expKey, existingTags))
		Expect(foundValue).To(Equal(expValue))
	}
}

func ExpectTagsNotFound(tags []*ec2.Tag, expectNotFound map[string]string) {
	GinkgoHelper()
	existingTags := lo.SliceToMap(tags, func(t *ec2.Tag) (string, string) { return *t.Key, *t.Value })
	for k, v := range expectNotFound {
		elem, ok := existingTags[k]
		Expect(!ok || v != elem).To(BeTrue())
	}
}

func ExpectLaunchTemplatesCreatedWithUserDataContaining(substrings ...string) {
	GinkgoHelper()
	Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
	awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(input *ec2.CreateLaunchTemplateInput) {
		userData, err := base64.StdEncoding.DecodeString(*input.LaunchTemplateData.UserData)
		ExpectWithOffset(2, err).To(BeNil())
		for _, substring := range substrings {
			ExpectWithOffset(2, string(userData)).To(ContainSubstring(substring))
		}
	})
}

func ExpectLaunchTemplatesCreatedWithUserDataNotContaining(substrings ...string) {
	GinkgoHelper()
	Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
	awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(input *ec2.CreateLaunchTemplateInput) {
		userData, err := base64.StdEncoding.DecodeString(*input.LaunchTemplateData.UserData)
		ExpectWithOffset(2, err).To(BeNil())
		for _, substring := range substrings {
			ExpectWithOffset(2, string(userData)).ToNot(ContainSubstring(substring))
		}
	})
}

func ExpectLaunchTemplatesCreatedWithUserData(expected string) {
	GinkgoHelper()
	Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
	awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(input *ec2.CreateLaunchTemplateInput) {
		userData, err := base64.StdEncoding.DecodeString(*input.LaunchTemplateData.UserData)
		ExpectWithOffset(2, err).To(BeNil())
		// Newlines are always added for missing TOML fields, so strip them out before comparisons.
		actualUserData := strings.Replace(string(userData), "\n", "", -1)
		expectedUserData := strings.Replace(expected, "\n", "", -1)
		ExpectWithOffset(2, actualUserData).To(Equal(expectedUserData))
	})
}

func ExpectUserDataExistsFromCreatedLaunchTemplates() []string {
	GinkgoHelper()
	Expect(awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.Len()).To(BeNumerically(">=", 1))
	userDatas := []string{}
	awsEnv.EC2API.CalledWithCreateLaunchTemplateInput.ForEach(func(input *ec2.CreateLaunchTemplateInput) {
		userData, err := base64.StdEncoding.DecodeString(*input.LaunchTemplateData.UserData)
		ExpectWithOffset(2, err).To(BeNil())
		userDatas = append(userDatas, string(userData))
	})
	return userDatas
}

func ExpectUserDataCreatedWithNodeConfigs(userData string) []admv1alpha1.NodeConfig {
	GinkgoHelper()
	archive, err := mime.NewArchive(userData)
	Expect(err).To(BeNil())
	nodeConfigs := lo.FilterMap([]mime.Entry(archive), func(entry mime.Entry, _ int) (admv1alpha1.NodeConfig, bool) {
		config := admv1alpha1.NodeConfig{}
		if entry.ContentType != mime.ContentTypeNodeConfig {
			return config, false
		}
		err := yaml.Unmarshal([]byte(entry.Content), &config)
		Expect(err).To(BeNil())
		return config, true
	})
	Expect(len(nodeConfigs)).To(BeNumerically(">=", 1))
	return nodeConfigs
}
