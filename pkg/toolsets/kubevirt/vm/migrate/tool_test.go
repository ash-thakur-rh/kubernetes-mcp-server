package migrate

import (
	"context"
	"testing"

	"github.com/containers/kubernetes-mcp-server/pkg/api"
	"github.com/containers/kubernetes-mcp-server/pkg/kubevirt"
	"github.com/stretchr/testify/suite"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

type MigrateToolSuite struct {
	suite.Suite
}

func TestMigrateToolSuite(t *testing.T) {
	suite.Run(t, new(MigrateToolSuite))
}

func (s *MigrateToolSuite) TestStartsMigrationForRunningVMI() {
	client := newMigrationClient(runningVMI())
	createdMigration := interceptMigrationCreate(client)

	result, err := invokeMigrate(client, map[string]any{
		"namespace": "vm-test",
		"name":      "test-vm",
	})

	s.Require().NoError(err)
	s.Require().Nil(result.Error)
	s.NotNil(result.StructuredContent)
	s.Contains(result.Content, "migration-test-vm")
	s.Require().NotNil(createdMigration())
	s.Equal("migration-test-vm", createdMigration().GetName())
	s.Equal("test-vm", nestedString(s.T(), createdMigration(), "spec", "vmiName"))
	s.False(unstructuredNestedFieldExists(createdMigration(), "spec", "addedNodeSelector"))
}

func (s *MigrateToolSuite) TestAddsDestinationNodeSelectorWhenRequested() {
	client := newMigrationClient(runningVMI())
	createdMigration := interceptMigrationCreate(client)

	result, err := invokeMigrate(client, map[string]any{
		"namespace":   "vm-test",
		"name":        "test-vm",
		"target_node": "worker-2",
	})

	s.Require().NoError(err)
	s.Require().Nil(result.Error)
	s.Require().NotNil(createdMigration())
	selector, found, err := unstructured.NestedStringMap(createdMigration().Object, "spec", "addedNodeSelector")
	s.Require().NoError(err)
	s.True(found)
	s.Equal(map[string]string{"kubernetes.io/hostname": "worker-2"}, selector)
}

func (s *MigrateToolSuite) TestRejectsVMIThatIsNotRunning() {
	vmi := runningVMI()
	if err := unstructured.SetNestedField(vmi.Object, "Scheduled", "status", "phase"); err != nil {
		s.FailNow("failed to configure test VMI", err.Error())
	}
	client := newMigrationClient(vmi)
	result, err := invokeMigrate(client, migrationArgs())

	s.Require().NoError(err)
	s.ErrorContains(result.Error, "must be Running")
	s.Zero(countMigrationCreates(client))
}

func (s *MigrateToolSuite) TestRejectsExplicitlyNonMigratableVMI() {
	vmi := runningVMI()
	if err := unstructured.SetNestedSlice(vmi.Object, []any{map[string]any{
		"type":    "LiveMigratable",
		"status":  "False",
		"reason":  "UnsupportedVolume",
		"message": "the VM uses a non-shared volume",
	}}, "status", "conditions"); err != nil {
		s.FailNow("failed to configure test VMI", err.Error())
	}
	client := newMigrationClient(vmi)
	result, err := invokeMigrate(client, migrationArgs())

	s.Require().NoError(err)
	s.ErrorContains(result.Error, "UnsupportedVolume: the VM uses a non-shared volume")
	s.Zero(countMigrationCreates(client))
}

func (s *MigrateToolSuite) TestReportsMissingVMI() {
	client := newMigrationClient()
	result, err := invokeMigrate(client, migrationArgs())

	s.Require().NoError(err)
	s.ErrorContains(result.Error, `VirtualMachine "test-vm" in namespace "vm-test" has no running VirtualMachineInstance`)
	s.Zero(countMigrationCreates(client))
}

func (s *MigrateToolSuite) TestRejectsTargetNodeWithWrongType() {
	client := newMigrationClient(runningVMI())
	result, err := invokeMigrate(client, map[string]any{
		"namespace":   "vm-test",
		"name":        "test-vm",
		"target_node": true,
	})

	s.Require().NoError(err)
	s.ErrorContains(result.Error, "target_node parameter must be a string")
	s.Zero(countMigrationCreates(client))
}

type migrateTestRequest struct {
	args map[string]any
}

func (r migrateTestRequest) GetArguments() map[string]any { return r.args }

type migrateTestClient struct {
	api.KubernetesClient
	dynamic dynamic.Interface
}

func (c migrateTestClient) DynamicClient() dynamic.Interface { return c.dynamic }

func invokeMigrate(client *dynamicfake.FakeDynamicClient, args map[string]any) (*api.ToolCallResult, error) {
	tool := Tools(context.Background(), nil)[0]
	return tool.Handler(api.ToolHandlerParams{
		Context:          context.Background(),
		Request:          migrateTestRequest{args: args},
		KubernetesClient: migrateTestClient{dynamic: client},
	})
}

func migrationArgs() map[string]any {
	return map[string]any{"namespace": "vm-test", "name": "test-vm"}
}

func runningVMI() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachineInstance",
		"metadata":   map[string]any{"name": "test-vm", "namespace": "vm-test"},
		"status":     map[string]any{"phase": "Running"},
	}}
}

func newMigrationClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), objects...)
}

func interceptMigrationCreate(client *dynamicfake.FakeDynamicClient) func() *unstructured.Unstructured {
	var created *unstructured.Unstructured
	client.PrependReactor("create", kubevirt.VirtualMachineInstanceMigrationGVR.Resource, func(action clienttesting.Action) (bool, runtime.Object, error) {
		migration := action.(clienttesting.CreateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		migration.SetName("migration-test-vm")
		created = migration
		return true, migration, nil
	})
	return func() *unstructured.Unstructured { return created }
}

func countMigrationCreates(client *dynamicfake.FakeDynamicClient) int {
	count := 0
	for _, action := range client.Actions() {
		if action.GetVerb() == "create" && action.GetResource() == kubevirt.VirtualMachineInstanceMigrationGVR {
			count++
		}
	}
	return count
}

func nestedString(t *testing.T, object *unstructured.Unstructured, fields ...string) string {
	t.Helper()
	value, found, err := unstructured.NestedString(object.Object, fields...)
	if err != nil || !found {
		t.Fatalf("expected string at %v, found=%t, err=%v", fields, found, err)
	}
	return value
}

func unstructuredNestedFieldExists(object *unstructured.Unstructured, fields ...string) bool {
	_, found, _ := unstructured.NestedFieldNoCopy(object.Object, fields...)
	return found
}
