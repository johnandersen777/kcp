package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/kube"
)

const (
	ReasonChildEditReverted = "ChildEditReverted"

	eventNamespace = "default"
)

type Desired struct {
	Kind string

	Name string

	Spec any
}

func DesiredKeys(desired []Desired) []string {
	out := make([]string, 0, len(desired))
	for _, d := range desired {
		out = append(out, ComponentKey(d.Kind, d.Name))
	}
	return out
}

func ToMap(obj any) (map[string]any, error) {
	if v := reflect.ValueOf(obj); v.IsValid() && v.Kind() != reflect.Pointer {
		ptr := reflect.New(v.Type())
		ptr.Elem().Set(v)
		obj = ptr.Interface()
	}
	return runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
}

func SpecMap(spec any) (map[string]any, error) {
	if spec == nil {
		return nil, nil
	}
	if m, ok := spec.(map[string]any); ok {
		return m, nil
	}
	m, err := ToMap(spec)
	if err != nil {
		return nil, failf("the component spec: %w", err)
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

type Generic struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec kube.Doc `json:"spec,omitempty"`

	Status kube.Doc `json:"status,omitempty"`
}

func DocMeta(d kube.Doc) map[string]any {
	m, _ := d["metadata"].(map[string]any)
	if m == nil {
		m = map[string]any{}
		d["metadata"] = m
	}
	return m
}

func docField(d kube.Doc, key string) string {
	s, _ := d[key].(string)
	return s
}

func docMetaField(d kube.Doc, key string) string {
	m, _ := d["metadata"].(map[string]any)
	s, _ := m[key].(string)
	return s
}

func NewDoc(apiVersion, kind, name string) kube.Doc {
	return kube.Doc{"apiVersion": apiVersion, "kind": kind, "metadata": map[string]any{"name": name}}
}

func (c *Composer) Object(r Root, ws, apiVersion, kind, name string, labels map[string]string) kube.Doc {
	d := NewDoc(apiVersion, kind, name)
	merged := r.Labels("")
	maps.Copy(merged, labels)
	meta := DocMeta(d)
	meta["labels"] = merged
	if ws == "" || ws == r.Workspace {
		meta["ownerReferences"] = []metav1.OwnerReference{r.OwnerRef()}
	}
	return d
}

func (c *Composer) ApplyComponent(ctx context.Context, r Root, ws string, d Desired) error {
	ct, err := componentTypeFor(d.Kind)
	if err != nil {
		return err
	}
	spec, err := SpecMap(d.Spec)
	if err != nil {
		return err
	}
	ws = workspaceOr(ws, r.Workspace)
	u := c.Object(r, ws, ct.APIVersion(), ct.Kind, d.Name, map[string]string{ComponentLabel: d.Kind})
	if spec != nil {
		u["spec"] = spec
	}
	if err := c.Apply(ctx, r, ws, ct.GVR, u); err != nil {
		return err
	}
	return c.revertForeignSpec(ctx, r, ws, ct.GVR, u)
}

func (c *Composer) EnsureComponents(ctx context.Context, r Root, desired []Desired) error {
	for _, d := range desired {
		if err := c.ApplyComponent(ctx, r, r.Workspace, d); err != nil {
			return fmt.Errorf("%s %s: %w", d.Kind, d.Name, err)
		}
	}
	return nil
}

func workspaceOr(ws, fallback string) string {
	if ws = strings.TrimSpace(ws); ws != "" {
		return ws
	}
	return fallback
}

func (c *Composer) Apply(ctx context.Context, r Root, ws string, gvr schema.GroupVersionResource, u kube.Doc) error {
	return c.applyAs(ctx, r, r.Manager(), ws, gvr, u)
}

func (c *Composer) applyAs(ctx context.Context, r Root, manager, ws string, gvr schema.GroupVersionResource, u kube.Doc) error {
	ws = workspaceOr(ws, r.Workspace)
	ri, err := c.Client(ws, gvr)
	if err != nil {
		return err
	}
	delete(u, "status")
	if m, ok := u["metadata"].(map[string]any); ok {
		delete(m, "creationTimestamp")
	}
	rc := ri.In(docMetaField(u, "namespace"))
	kind, name := docField(u, "kind"), docMetaField(u, "name")
	_, err = kube.Apply[kube.Doc](ctx, rc, u, kube.ApplyOptions{FieldManager: manager})
	if err == nil {
		return nil
	}
	if !apierrors.IsConflict(err) {
		return fmt.Errorf("compose: applying %s %s in %s: %w", kind, name, ws, err)
	}
	c.recordConflict(ctx, r, ws, u, err)
	if _, err := kube.Apply[kube.Doc](ctx, rc, u, kube.ApplyOptions{FieldManager: manager, Force: true}); err != nil {
		return fmt.Errorf("compose: applying %s %s in %s: %w", kind, name, ws, err)
	}
	return nil
}

func (c *Composer) recordConflict(ctx context.Context, r Root, ws string, u kube.Doc, cause error) {
	kind, name := docField(u, "kind"), docMetaField(u, "name")
	message := fmt.Sprintf("%s %s: a field another manager set is reverted to the spec %s %s composes: %s", kind, name, r.Kind, r.Name, cause.Error())
	c.log.Warn("a direct edit of a composed child is reverted", "root", r.Owner(), "kind", kind, "name", name, "workspace", ws, "err", cause.Error())
	c.Event(ctx, r, ws, docField(u, "apiVersion"), kind, name, "Warning", ReasonChildEditReverted, message)
}

func (c *Composer) Event(ctx context.Context, r Root, ws, apiVersion, kind, name, eventType, reason, message string) {
	ri, err := c.Client(ws, EventGVR)
	if err != nil {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if len(message) > 1024 {
		message = message[:1024]
	}
	ev := kube.Doc{
		"apiVersion":         "v1",
		"kind":               "Event",
		"metadata":           map[string]any{"generateName": strings.ToLower(kind) + "-" + name + "-", "namespace": eventNamespace},
		"involvedObject":     map[string]any{"apiVersion": apiVersion, "kind": kind, "name": name},
		"reason":             reason,
		"message":            message,
		"type":               eventType,
		"source":             map[string]any{"component": r.ManagedBy()},
		"reportingComponent": r.ManagedBy(),
		"firstTimestamp":     now,
		"lastTimestamp":      now,
		"count":              int64(1),
	}
	if _, err := kube.Create[kube.Doc](ctx, ri.In(eventNamespace), ev, metav1.CreateOptions{FieldManager: r.Manager()}); err != nil && ctx.Err() == nil {
		c.log.Warn("recording an event", "root", r.Owner(), "reason", reason, "err", err.Error())
	}
}

func (c *Composer) ApplyTyped(ctx context.Context, r Root, ws string, gvr schema.GroupVersionResource, apiVersion, kind string, obj any) error {
	m, err := ToMap(obj)
	if err != nil {
		return err
	}
	u := kube.Doc(m)
	u["apiVersion"], u["kind"] = apiVersion, kind
	ws = workspaceOr(ws, r.Workspace)
	meta := DocMeta(u)
	labels := r.Labels(kind)
	if own, ok := meta["labels"].(map[string]any); ok {
		for k, v := range own {
			if s, ok := v.(string); ok {
				labels[k] = s
			}
		}
	}
	meta["labels"] = labels
	if ws == r.Workspace {
		meta["ownerReferences"] = []metav1.OwnerReference{r.OwnerRef()}
	}
	return c.Apply(ctx, r, ws, gvr, u)
}

func Get[T any](ctx context.Context, c *Composer, ws string, gvr schema.GroupVersionResource, name string) (*T, error) {
	ri, err := c.Client(ws, gvr)
	if err != nil {
		return nil, err
	}
	return kube.Get[T](ctx, ri, name)
}

func (c *Composer) GetComponent(ctx context.Context, ws, kind, name string) (ComponentView, bool, error) {
	ct, err := componentTypeFor(kind)
	if err != nil {
		return ComponentView{}, false, err
	}
	o, err := Get[Generic](ctx, c, ws, ct.GVR, name)
	if apierrors.IsNotFound(err) {
		return ComponentView{Kind: kind, Name: name, Workspace: ws}, false, nil
	}
	if err != nil {
		return ComponentView{}, false, err
	}
	v, err := ComponentViewOf(ws, kind, o)
	return v, err == nil, err
}

func (c *Composer) ListComponents(ctx context.Context, ws, owner string) ([]ComponentView, error) {
	var out []ComponentView
	for _, ct := range ComponentTypes {
		ri, err := c.Client(ws, ct.GVR)
		if err != nil {
			return nil, err
		}
		items, err := kube.List[Generic](ctx, ri, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + owner})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for i := range items {
			v, err := ComponentViewOf(ws, ct.Kind, &items[i])
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (c *Composer) DeleteComponent(ctx context.Context, ws, kind, name string) error {
	ct, err := componentTypeFor(kind)
	if err != nil {
		return err
	}
	ri, err := c.Client(ws, ct.GVR)
	if err != nil {
		return err
	}
	if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (c *Composer) DeleteComponentsExcept(ctx context.Context, ws, owner string, keep []string) (int, error) {
	views, err := c.ListComponents(ctx, ws, owner)
	if err != nil {
		return 0, err
	}
	left := 0
	for _, v := range views {
		if slices.Contains(keep, ComponentKey(v.Kind, v.Name)) {
			continue
		}
		left++
		if err := c.DeleteComponent(ctx, ws, v.Kind, v.Name); err != nil {
			return left, err
		}
		c.log.Info("deleted a component the root object omits", "owner", owner, "kind", v.Kind, "name", v.Name, "workspace", ws)
	}
	return left, nil
}

func (c *Composer) RestartComponent(ctx context.Context, r Root, ws, kind, name string) error {
	ct, err := componentTypeFor(kind)
	if err != nil {
		return err
	}
	u := NewDoc(ct.APIVersion(), ct.Kind, name)
	u["spec"] = map[string]any{"restartedAt": time.Now().UTC().Format(time.RFC3339Nano)}
	return c.applyAs(ctx, r, r.Manager()+"/restart", ws, ct.GVR, u)
}

func (c *Composer) Prune(ctx context.Context, ws string, gvr schema.GroupVersionResource, selector string, keep map[string]bool) error {
	ri, err := c.Client(ws, gvr)
	if err != nil {
		return err
	}
	items, err := kube.List[metav1.PartialObjectMetadata](ctx, ri, metav1.ListOptions{LabelSelector: selector})
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i := range items {
		name := items[i].Name
		if keep[name] {
			continue
		}
		if err := ri.Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (c *Composer) DeleteOwned(ctx context.Context, ws string, gvr schema.GroupVersionResource, owner string) (int, error) {
	ri, err := c.Client(ws, gvr)
	if err != nil {
		return 0, err
	}
	items, err := kube.List[metav1.PartialObjectMetadata](ctx, ri, metav1.ListOptions{LabelSelector: OwnerLabel + "=" + owner})
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, o := range items {
		if o.DeletionTimestamp == nil {
			if err := ri.Delete(ctx, o.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return len(items), err
			}
		}
	}
	return len(items), nil
}

func (c *Composer) DeleteAll(ctx context.Context, ws string, gvr schema.GroupVersionResource) (int, error) {
	ri, err := c.Client(ws, gvr)
	if err != nil {
		return 0, err
	}
	items, err := kube.List[metav1.PartialObjectMetadata](ctx, ri, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	for _, o := range items {
		if o.DeletionTimestamp == nil {
			if err := ri.Delete(ctx, o.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				return len(items), err
			}
		}
	}
	return len(items), nil
}

func (c *Composer) HeldContracts(ctx context.Context, ws string) (int, error) {
	ri, err := c.Client(ws, ComputeContractGVR)
	if err != nil {
		return 0, err
	}
	items, err := kube.List[v1alpha1.ComputeContract](ctx, ri, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	held := 0
	for i := range items {
		if items[i].HoldsGuest() {
			held++
		}
	}
	return held, nil
}

func (c *Composer) SetSpecField(ctx context.Context, r Root, ws string, gvr schema.GroupVersionResource, apiVersion, kind, name string, spec map[string]any) error {
	u := NewDoc(apiVersion, kind, name)
	u["spec"] = spec
	return c.Apply(ctx, r, ws, gvr, u)
}

func jsonCopy(in any, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
