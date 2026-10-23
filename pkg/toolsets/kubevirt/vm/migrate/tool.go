package migrate

import (
	"context"
	"fmt"

	"github.com/containers/kubernetes-mcp-server/pkg/api"
	"github.com/containers/kubernetes-mcp-server/pkg/kubevirt"
	"github.com/containers/kubernetes-mcp-server/pkg/toolsets/kubevirt/internal/defaults"
	"github.com/google/jsonschema-go/jsonschema"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
)

func Tools(ctx context.Context, inspector api.ClusterInspector) []api.ServerTool {
	return []api.ServerTool{{
		Tool: api.Tool{
			Name:        "vm_migrate",
			Description: fmt.Sprintf("Start a live migration of a running %s VirtualMachine. Use this to relocate a VM while its guest workload remains available; KubeVirt chooses an eligible destination unless target_node is specified. The migration runs asynchronously, so the returned resource shows its current status rather than guaranteeing completion.", defaults.ProductName()),
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"namespace": {
						Type:        "string",
						Description: "The namespace of the VirtualMachine",
					},
					"name": {
						Type:        "string",
						Description: "The name of the running VirtualMachine to migrate",
					},
					"target_node": {
						Type:        "string",
						Description: "Optional Kubernetes node name to target. When omitted, KubeVirt selects an eligible destination node.",
					},
				},
				Required: []string{"namespace", "name"},
			},
			Annotations: api.ToolAnnotations{
				Title:           "Virtual Machine: Migrate",
				ReadOnlyHint:    ptr.To(false),
				DestructiveHint: ptr.To(true),
				IdempotentHint:  ptr.To(false),
				OpenWorldHint:   ptr.To(true),
			},
		},
		RBAC: api.RBACBounded(
			api.RBACRequirement{
				Verbs:        []string{"get"},
				Target:       api.RBACTarget{Resource: &api.RBACResourceTarget{APIGroup: "kubevirt.io", Resource: "virtualmachineinstances"}},
				Namespace:    &api.RBACNamespace{Argument: "namespace"},
				ResourceName: &api.RBACResourceName{Argument: "name"},
			},
			api.RBACRequirement{
				Verbs:     []string{"create"},
				Target:    api.RBACTarget{Resource: &api.RBACResourceTarget{APIGroup: "kubevirt.io", Resource: "virtualmachineinstancemigrations"}},
				Namespace: &api.RBACNamespace{Argument: "namespace"},
			},
		),
		Handler: migrate,
		TargetCompatibilityFilters: []func() bool{
			kubevirt.HasVirtualMachine(ctx, inspector),
		},
	}}
}

func migrate(params api.ToolHandlerParams) (*api.ToolCallResult, error) {
	p := api.WrapParams(params)
	namespace := p.RequiredString("namespace")
	name := p.RequiredString("name")
	targetNode := p.OptionalString("target_node", "")
	if err := p.Err(); err != nil {
		return api.NewToolCallResult("", err), nil
	}

	dynamicClient := params.DynamicClient()
	vmi, err := dynamicClient.Resource(kubevirt.VirtualMachineInstanceGVR).
		Namespace(namespace).
		Get(params.Context, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return api.NewToolCallResult("", fmt.Errorf("VirtualMachine %q in namespace %q has no running VirtualMachineInstance", name, namespace)), nil
		}
		return api.NewToolCallResult("", fmt.Errorf("failed to get VirtualMachineInstance %q in namespace %q: %w", name, namespace, err)), nil
	}

	phase, found, err := unstructured.NestedString(vmi.Object, "status", "phase")
	if err != nil {
		return api.NewToolCallResult("", fmt.Errorf("failed to read VirtualMachineInstance phase: %w", err)), nil
	}
	if !found || phase != "Running" {
		return api.NewToolCallResult("", fmt.Errorf("VirtualMachineInstance %q in namespace %q must be Running to migrate (current phase: %q)", name, namespace, phase)), nil
	}
	if reason := nonMigratableReason(vmi); reason != "" {
		return api.NewToolCallResult("", fmt.Errorf("VirtualMachineInstance %q in namespace %q is not live migratable: %s", name, namespace, reason)), nil
	}

	migrationSpec := map[string]any{
		"vmiName": name,
	}
	if targetNode != "" {
		migrationSpec["addedNodeSelector"] = map[string]any{
			"kubernetes.io/hostname": targetNode,
		}
	}

	migration := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachineInstanceMigration",
		"metadata": map[string]any{
			"generateName": "migration-",
			"namespace":    namespace,
		},
		"spec": migrationSpec,
	}}
	created, err := dynamicClient.Resource(kubevirt.VirtualMachineInstanceMigrationGVR).
		Namespace(namespace).
		Create(params.Context, migration, metav1.CreateOptions{})
	if err != nil {
		return api.NewToolCallResult("", fmt.Errorf("failed to request live migration for VirtualMachine %q in namespace %q: %w", name, namespace, err)), nil
	}

	return api.NewToolCallResultStructured(map[string]any{
		"message":   "Migration request created. KubeVirt performs migration asynchronously; check the migration resource status to follow progress.",
		"migration": created.Object,
	}, nil), nil
}

func nonMigratableReason(vmi *unstructured.Unstructured) string {
	conditions, found, err := unstructured.NestedSlice(vmi.Object, "status", "conditions")
	if err != nil || !found {
		return ""
	}
	for _, item := range conditions {
		condition, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typeName, _, _ := unstructured.NestedString(condition, "type")
		status, _, _ := unstructured.NestedString(condition, "status")
		if typeName != "LiveMigratable" || status != "False" {
			continue
		}
		reason, _, _ := unstructured.NestedString(condition, "reason")
		message, _, _ := unstructured.NestedString(condition, "message")
		if reason != "" && message != "" {
			return reason + ": " + message
		}
		if message != "" {
			return message
		}
		if reason != "" {
			return reason
		}
		return "the LiveMigratable condition is False"
	}
	return ""
}
