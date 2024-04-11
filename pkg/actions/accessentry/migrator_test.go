package accessentry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/kris-nova/logger"
	"github.com/stretchr/testify/mock"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/aws/aws-sdk-go-v2/aws"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	awseks "github.com/aws/aws-sdk-go-v2/service/eks"
	ekstypes "github.com/aws/aws-sdk-go-v2/service/eks/types"
	awsiam "github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/weaveworks/eksctl/pkg/actions/accessentry"
	"github.com/weaveworks/eksctl/pkg/actions/acessentry/fakes"
	"github.com/weaveworks/eksctl/pkg/actions/podidentityassociation"
	api "github.com/weaveworks/eksctl/pkg/apis/eksctl.io/v1alpha5"
	"github.com/weaveworks/eksctl/pkg/cfn/manager"
	"github.com/weaveworks/eksctl/pkg/testutils/mockprovider"
)

type migrateToAccessEntryEntry struct {
	mockEKS                    func(provider *mockprovider.MockProvider)
	mockK8s                    func(clientSet *fake.Clientset)
	mockAeCreator              func(creator *fakes.FakeCreatorInterface)
	validateCustomLoggerOutput func(output string)
	options                    accessentry.AccessEntryMigrationOptions
	expectedErr                string
}

var _ = Describe("Migrate Access Entry", func() {
	var (
		migrator         *accessentry.Migrator
		mockProvider     *mockprovider.MockProvider
		fakeStackUpdater *fakes.FakeStackUpdater
		fakeClientset    *fake.Clientset

		clusterName = "test-cluster"
		genericErr = fmt.Errorf("ERR")
	)

	DescribeTable("Create", func(e migrateToAccessEntryEntry) {

		mockProvider = mockprovider.NewMockProvider()
		if e.mockEKS != nil {
			e.mockEKS(mockProvider)
		}

		fakeClientset = fake.NewSimpleClientset()
		if e.mockK8s != nil {
			e.mockK8s(fakeClientset)
		}

		output := &bytes.Buffer{}
		if e.validateCustomLoggerOutput != nil {
			defer func() {
				logger.Writer = os.Stdout
			}()
			logger.Writer = output
		}

		aeCreator := e.mockAeCreator

		migrator = podidentityassociation.NewMigrator(
			clusterName,
			mockProvider.MockEKS(),
			mockProvider.MockIAM(),
			fakeStackUpdater,
			fakeClientset,
			addonCreator,
		)

		err = migrator.MigrateToAccessEntry(context.Background(), e.options)
		if e.expectedErr != "" {
			Expect(err).To(MatchError(ContainSubstring(e.expectedErr)))
			return
		}
		Expect(err).ToNot(HaveOccurred())

		if e.validateCustomLoggerOutput != nil {
			e.validateCustomLoggerOutput(output.String())
		}
	},
		Entry("[API errors] describing pod identity agent addon fails", migrateToPodIdentityAssociationEntry{
			mockEKS: func(provider *mockprovider.MockProvider) {
				mockDescribeAddon(provider, genericErr)
			},
			expectedErr: fmt.Sprintf("calling %q", fmt.Sprintf("EKS::DescribeAddon::%s", api.PodIdentityAgentAddon)),
		}),

		Entry("[API errors] fetching iamserviceaccounts fails", migrateToPodIdentityAssociationEntry{
			mockEKS: func(provider *mockprovider.MockProvider) {
				mockDescribeAddon(provider, nil)
			},
			mockCFN: func(stackUpdater *fakes.FakeStackUpdater) {
				stackUpdater.GetIAMServiceAccountsReturns(nil, genericErr)
			},
			expectedErr: "getting iamserviceaccount role stacks",
		}),

		Entry("[taskTree] contains a task to create pod identity agent addon if not already installed", migrateToPodIdentityAssociationEntry{
			mockEKS: func(provider *mockprovider.MockProvider) {
				mockDescribeAddon(provider, &ekstypes.ResourceNotFoundException{
					Message: aws.String(genericErr.Error()),
				})
			},
			mockCFN: func(stackUpdater *fakes.FakeStackUpdater) {
				stackUpdater.GetIAMServiceAccountsReturns([]*api.ClusterIAMServiceAccount{}, nil)
			},
			mockK8s: func(clientSet *fake.Clientset) {
				createFakeServiceAccount(clientSet, nsDefault, sa1, roleARN1)
			},
			validateCustomLoggerOutput: func(output string) {
				Expect(output).To(ContainSubstring(fmt.Sprintf("install %s addon", api.PodIdentityAgentAddon)))
			},
		}),

		Entry("[taskTree] contains tasks to remove IRSAv1 EKS Role annotation if remove trust option is specified", migrateToPodIdentityAssociationEntry{
			mockEKS: func(provider *mockprovider.MockProvider) {
				mockDescribeAddon(provider, nil)
			},
			mockCFN: func(stackUpdater *fakes.FakeStackUpdater) {
				stackUpdater.GetIAMServiceAccountsReturns([]*api.ClusterIAMServiceAccount{}, nil)
			},
			mockK8s: func(clientSet *fake.Clientset) {
				createFakeServiceAccount(clientSet, nsDefault, sa1, roleARN1)
			},
			validateCustomLoggerOutput: func(output string) {
				Expect(output).To(ContainSubstring("remove iamserviceaccount EKS role annotation for \"default/service-account-1\""))
			},
			options: podidentityassociation.PodIdentityMigrationOptions{
				RemoveOIDCProviderTrustRelationship: true,
			},
		}),

		Entry("[taskTree] contains all other expected tasks", migrateToPodIdentityAssociationEntry{
			mockEKS: func(provider *mockprovider.MockProvider) {
				mockDescribeAddon(provider, nil)
			},
			mockCFN: func(stackUpdater *fakes.FakeStackUpdater) {
				stackUpdater.GetIAMServiceAccountsReturns([]*api.ClusterIAMServiceAccount{
					{
						Status: &api.ClusterIAMServiceAccountStatus{
							RoleARN: aws.String(roleARN1),
							StackName: aws.String(makeIRSAv2StackName(podidentityassociation.Identifier{
								Namespace:          nsDefault,
								ServiceAccountName: sa1,
							})),
						},
					},
				}, nil)
			},
			mockK8s: func(clientSet *fake.Clientset) {
				createFakeServiceAccount(clientSet, nsDefault, sa1, roleARN1)
				createFakeServiceAccount(clientSet, nsDefault, sa2, roleARN2)
			},
			validateCustomLoggerOutput: func(output string) {
				Expect(output).To(ContainSubstring("update trust policy for owned role \"test-role-1\""))
				Expect(output).To(ContainSubstring("update trust policy for unowned role \"test-role-2\""))
				Expect(output).To(ContainSubstring("create pod identity association for service account \"default/service-account-1\""))
				Expect(output).To(ContainSubstring("create pod identity association for service account \"default/service-account-2\""))
			},
		}),

		Entry("completes all tasks successfully", migrateToPodIdentityAssociationEntry{
			mockEKS: func(provider *mockprovider.MockProvider) {
				mockDescribeAddon(provider, nil)

				mockProvider.MockEKS().
					On("CreatePodIdentityAssociation", mock.Anything, mock.Anything).
					Run(func(args mock.Arguments) {
						Expect(args).To(HaveLen(2))
						Expect(args[1]).To(BeAssignableToTypeOf(&awseks.CreatePodIdentityAssociationInput{}))
					}).
					Return(nil, nil).
					Twice()

				mockProvider.MockIAM().
					On("GetRole", mock.Anything, mock.Anything).
					Return(&awsiam.GetRoleOutput{
						Role: &iamtypes.Role{
							AssumeRolePolicyDocument: policyDocument,
						},
					}, nil).
					Twice()

				mockProvider.MockIAM().
					On("UpdateAssumeRolePolicy", mock.Anything, mock.Anything).
					Run(func(args mock.Arguments) {
						Expect(args).To(HaveLen(2))
						Expect(args[1]).To(BeAssignableToTypeOf(&awsiam.UpdateAssumeRolePolicyInput{}))
						input := args[1].(*awsiam.UpdateAssumeRolePolicyInput)

						var trustPolicy api.IAMPolicyDocument
						Expect(json.Unmarshal([]byte(*input.PolicyDocument), &trustPolicy)).NotTo(HaveOccurred())
						Expect(trustPolicy.Statements).To(HaveLen(1))
						value, exists := trustPolicy.Statements[0].Principal["Service"]
						Expect(exists).To(BeTrue())
						Expect(value).To(ConsistOf([]string{api.EKSServicePrincipal}))
					}).
					Return(nil, nil).
					Once()
			},
			mockCFN: func(stackUpdater *fakes.FakeStackUpdater) {
				stackUpdater.GetIAMServiceAccountsReturns([]*api.ClusterIAMServiceAccount{
					{
						Status: &api.ClusterIAMServiceAccountStatus{
							RoleARN: aws.String(roleARN1),
							StackName: aws.String(makeIRSAv1StackName(podidentityassociation.Identifier{
								Namespace:          nsDefault,
								ServiceAccountName: sa1,
							})),
							Capabilities: []string{"CAPABILITY_IAM"},
						},
					},
				}, nil)

				stackUpdater.MustUpdateStackStub = func(ctx context.Context, options manager.UpdateStackOptions) error {
					Expect(options.Stack).NotTo(BeNil())
					Expect(options.Stack.Tags).To(ConsistOf([]cfntypes.Tag{
						{
							Key:   aws.String(api.PodIdentityAssociationNameTag),
							Value: aws.String(nsDefault + "/" + sa1),
						},
					}))
					Expect(options.Stack.Capabilities).To(ConsistOf([]cfntypes.Capability{"CAPABILITY_IAM"}))
					return nil
				}
			},
			mockK8s: func(clientSet *fake.Clientset) {
				createFakeServiceAccount(clientSet, nsDefault, sa1, roleARN1)
				createFakeServiceAccount(clientSet, nsDefault, sa2, roleARN2)
			},
			options: podidentityassociation.PodIdentityMigrationOptions{
				RemoveOIDCProviderTrustRelationship: true,
				Approve:                             true,
			},
		}),
	)
})
