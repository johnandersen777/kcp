package compose

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	pdsv1alpha1 "github.com/johnandersen777/socialweb-computer-kcp/api/pds/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/bus"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/contract/atprotorepo"
)

const (
	EnvPdsAccountPassword = "ATPROTO_PASSWORD"

	AssociationWatch = "association"

	AssociateConfirmNSID = "com.publicdomainrelay.temp.requester.associateConfirm"
	BadgeBlueKeysNSID    = "com.publicdomainrelay.temp.badgeBlueKeys"
	BidderAssociate      = "bidder_associate"
	RequesterAssociate   = "requester_associate"
)

var ErrMissingPdsAccountPassword = errors.New("compose: mode pdsAccount needs the account password in " +
	EnvPdsAccountPassword + " or --atproto-password; no password is available")

type AccountVault struct {
	Server string

	Client string

	Mount string
}

func (v AccountVault) MountOr(fallback string) string {
	return cmp.Or(strings.Trim(strings.TrimSpace(v.Mount), "/"), fallback)
}

type AccountPlan struct {
	Mode v1alpha1.MarketIdentityMode

	Local bool

	Identity v1alpha1.MarketIdentitySpec

	PdsRef *commonv1.ObjectRef

	RemotePDS *pdsv1alpha1.RemotePDS

	PdsURL string

	SessionPath string

	KeysPath string

	ComponentMode string

	MarketExposure *commonv1.LocalObjectRef

	PdsDID string

	Vault AccountVault
}

func (p AccountPlan) AccessToken() *commonv1.SecretRef {
	if p.SessionPath == "" {
		return nil
	}
	// The browser credential resolver requires a mount. Native code lets the
	// vault ref default it later; the memory store cannot.
	return &commonv1.SecretRef{Mount: p.Vault.MountOr("requester"), Path: p.SessionPath}
}

func (p AccountPlan) sessionSecret() *commonv1.SecretRef {
	if p.Identity.SessionRef == nil || strings.TrimSpace(p.Vault.Server) == "" {
		return nil
	}
	ref := *p.Identity.SessionRef
	vault := AppRef(KindVault, strings.TrimSpace(p.Vault.Server), "")
	ref.Vault = &vault
	return &ref
}

func (p AccountPlan) ComponentPdsRef() *commonv1.ObjectRef {
	if p.Local {
		return ComponentRefFor(ComponentKindPds, PdsComponent)
	}
	return p.PdsRef
}

func (p AccountPlan) UsesAssociation() bool {
	return p.Local
}

type accountBinding struct {
	DID string

	PDSRef *commonv1.ObjectRef

	AuthRef *commonv1.SecretRef

	RemotePDS *pdsv1alpha1.RemotePDS

	OwnsAuth bool
}

func (p AccountPlan) binding(did, pdsURL string) (accountBinding, error) {
	binding := accountBinding{DID: strings.TrimSpace(did)}
	if p.Local {
		binding.PDSRef = &commonv1.ObjectRef{Kind: ComponentKindPds, Name: PdsComponent}
		return binding, nil
	}
	binding.AuthRef = p.sessionSecret()
	if p.RemotePDS != nil && p.RemotePDS.Name != "" {
		binding.RemotePDS = p.RemotePDS
	} else if p.PdsRef != nil && strings.EqualFold(p.PdsRef.Kind, KindRemotePDS) && p.PdsRef.Name != "" {
		binding.RemotePDS = &pdsv1alpha1.RemotePDS{ObjectMeta: metav1.ObjectMeta{Name: p.PdsRef.Name}}
	}
	if u := FirstNonEmpty(p.PdsURL, p.Identity.PdsURL, pdsURL); binding.RemotePDS == nil && u != "" {
		binding.RemotePDS = &pdsv1alpha1.RemotePDS{ObjectMeta: metav1.ObjectMeta{Name: deterministicPdsName(u)},
			Spec: pdsv1alpha1.RemotePDSSpec{URL: u, DID: pdsServiceDID(u)}}
	}
	if binding.RemotePDS != nil {
		if binding.AuthRef != nil && binding.RemotePDS.Spec.Auth == (pdsv1alpha1.RemotePDSAuth{}) {
			refresh := *binding.AuthRef
			binding.RemotePDS.Spec.Auth.AccessTokenRef = binding.AuthRef
			binding.RemotePDS.Spec.Auth.RefreshTokenRef = &refresh
			binding.OwnsAuth = true
		}
		binding.PDSRef = &commonv1.ObjectRef{Kind: KindRemotePDS, Name: binding.RemotePDS.Name}
		return binding, nil
	}
	return accountBinding{}, errors.New("compose: the existing-account repo names no PDS")
}

func deterministicPdsName(u string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(u)))
	return "pds-" + hex.EncodeToString(sum[:8])
}

func (c *Composer) PlanAccount(ctx context.Context, ws string, id v1alpha1.ApplicationIdentity,
	suffix string, vault AccountVault, defaultAssociation v1alpha1.AssociationMode) (AccountPlan, error) {
	mode := cmp.Or(id.Mode, v1alpha1.MarketIdentityModeLocalRepo)
	mount := vault.MountOr(suffix)
	sessionPath := "identity/" + suffix + "/pds-session"
	keysPath := "identity/" + suffix + "/keys"
	plan := AccountPlan{Mode: mode, SessionPath: sessionPath, KeysPath: keysPath, Vault: vault}
	approved := slices.Clone(id.ApprovedAssociations)
	association := cmp.Or(id.Association, defaultAssociation, v1alpha1.AssociationRequireApproval)
	switch mode {
	case v1alpha1.MarketIdentityModeLocalRepo:
		plan.Local = true
		plan.ComponentMode = "local"
		plan.Identity = v1alpha1.MarketIdentitySpec{Mode: mode, Handle: id.Handle, PdsURL: id.PdsURL, Association: association,
			ApprovedAssociations: approved}
		plan.SessionPath = ""
		plan.KeysPath = ""
		return plan, nil
	case v1alpha1.MarketIdentityModePdsAccount:
		plan.ComponentMode = "callback"
		plan.Identity = v1alpha1.MarketIdentitySpec{Mode: mode, Handle: id.Handle, PdsURL: id.PdsURL,
			KeysRef:              &commonv1.SecretRef{Mount: mount, Path: keysPath},
			SessionRef:           &commonv1.SecretRef{Mount: mount, Path: sessionPath},
			Association:          association,
			ApprovedAssociations: approved}
		u, remote, err := c.resolveAccountPds(ctx, ws, id)
		if err != nil {
			return AccountPlan{}, err
		}
		plan.PdsURL = u
		plan.RemotePDS = remote
		plan.Identity.PdsURL = u
		if remote != nil {
			plan.PdsRef, plan.PdsDID = &commonv1.ObjectRef{Kind: KindRemotePDS, Name: remote.Name}, remote.Spec.DID
		}
		return plan, nil
	case v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthDelegated:
		plan.ComponentMode = "oauth"
		plan.Identity = v1alpha1.MarketIdentitySpec{Mode: mode, Handle: id.Handle, PdsURL: id.PdsURL,
			SessionRef: &commonv1.SecretRef{Mount: mount, Path: sessionPath}, Association: association, ApprovedAssociations: approved}
		plan.PdsURL = strings.TrimSpace(id.PdsURL)
		remote, err := c.remotePDSRef(ctx, ws, id.PdsRef)
		if err != nil {
			return AccountPlan{}, err
		}
		if remote != nil {
			plan.RemotePDS = remote
			plan.PdsRef, plan.PdsDID = &commonv1.ObjectRef{Kind: KindRemotePDS, Name: remote.Name}, remote.Spec.DID
			plan.PdsURL = cmp.Or(plan.PdsURL, remote.Spec.URL)
			plan.Identity.PdsURL = cmp.Or(plan.Identity.PdsURL, remote.Spec.URL)
		}
		return plan, nil
	}
	return AccountPlan{}, fmt.Errorf("compose: MarketIdentity mode %q: want one of localRepo, pdsAccount, oauthBrowser, oauthQr, oauthDelegated", mode)
}

func (c *Composer) SeedPdsAccountPassword(ctx context.Context, r Root, plan AccountPlan) error {
	if plan.Mode != v1alpha1.MarketIdentityModePdsAccount {
		return nil
	}
	password := strings.TrimSpace(os.Getenv(EnvPdsAccountPassword))
	if password == "" {
		return ErrMissingPdsAccountPassword
	}
	if plan.Identity.KeysRef == nil || strings.TrimSpace(plan.KeysPath) == "" {
		return errors.New("compose: mode pdsAccount names no keysRef for its password")
	}
	if plan.Vault.Server == "" {
		return errors.New("compose: mode pdsAccount names no vault for its password")
	}
	mount := plan.Vault.MountOr(plan.Vault.Client)
	kv, err := c.KV(ctx, r, plan.Vault.Server, mount+"/identity/*")
	if err != nil {
		return err
	}
	data, err := readKV(ctx, kv, mount, plan.KeysPath)
	if err != nil {
		return err
	}
	if existing, _ := data["password"].(string); existing == password {
		return nil
	}
	data["password"] = password
	if err := kv.Write(ctx, mount, plan.KeysPath, data); err != nil {
		return err
	}
	c.log.Info("the pdsAccount password is in its vault keysRef", "vault", plan.Vault.Server, "path", plan.KeysPath)
	return nil
}

func (c *Composer) resolveAccountPds(ctx context.Context, ws string, id v1alpha1.ApplicationIdentity) (string, *pdsv1alpha1.RemotePDS, error) {
	remote, err := c.remotePDSRef(ctx, ws, id.PdsRef)
	if err != nil {
		return "", nil, err
	}
	if u := strings.TrimSpace(id.PdsURL); u != "" {
		return u, remote, nil
	}
	if remote == nil {
		return "", nil, fmt.Errorf("compose: mode pdsAccount names no pdsUrl and no RemotePDS ref; it fails closed instead of using a local repo")
	}
	if strings.TrimSpace(remote.Spec.URL) == "" {
		return "", nil, fmt.Errorf("compose: RemotePDS %s names no url; mode pdsAccount fails closed", remote.Name)
	}
	return strings.TrimSpace(remote.Spec.URL), remote, nil
}

func (c *Composer) remotePDSRef(ctx context.Context, ws string, ref *commonv1.ObjectRef) (*pdsv1alpha1.RemotePDS, error) {
	if ref == nil || !strings.EqualFold(strings.TrimSpace(ref.Kind), KindRemotePDS) || strings.TrimSpace(ref.Name) == "" {
		return nil, nil
	}
	target := cmp.Or(strings.TrimSpace(ref.Workspace), ws)
	object, err := Get[pdsv1alpha1.RemotePDS](ctx, c, target, RemotePDSGVR, strings.TrimSpace(ref.Name))
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("compose: RemotePDS %s is absent in workspace %s; the existing-account mode fails closed", ref.Name, target)
		}
		return nil, err
	}
	return object, nil
}

func (c *Composer) DeleteAssociationObjects(ctx context.Context, ws string) {
	for _, gvr := range []schema.GroupVersionResource{AtprotoRecordGVR, RecordWatchGVR} {
		ri, err := c.Client(ws, gvr)
		if err != nil {
			continue
		}
		if err := ri.Delete(ctx, AssociationWatch, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			c.log.Warn("removing an association object", "resource", gvr.Resource, "err", err.Error())
		}
	}
}

func pdsServiceDID(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || net.ParseIP(parsed.Hostname()) != nil {
		return ""
	}
	return "did:web:" + strings.ReplaceAll(strings.ToLower(parsed.Host), ":", "%3A")
}

func (c *Composer) Identity(ctx context.Context, ws, name string) (*v1alpha1.MarketIdentity, error) {
	return Get[v1alpha1.MarketIdentity](ctx, c, ws, MarketIdentityGVR, name)
}

func (c *Composer) IdentityDid(ctx context.Context, ws, name string) string {
	id, err := c.Identity(ctx, ws, name)
	if err != nil {
		return ""
	}
	return id.Status.Did
}

func OperatorWatched(s v1alpha1.MarketIdentitySpec, operator, service string) v1alpha1.MarketIdentitySpec {
	if s.Mode == v1alpha1.MarketIdentityModeLocalRepo && s.Association != v1alpha1.AssociationSkip {
		s.AssociateWith, s.AssociationWatch = operator, AssociationWatch
		s.AssociationService = service
	}
	return s
}

func (c *Composer) ApplyIdentitySpec(ctx context.Context, r Root, ws, name string, spec v1alpha1.MarketIdentitySpec) error {
	if spec.Mode == v1alpha1.MarketIdentityModeLocalRepo && spec.Did == "" {
		if live, err := c.Identity(ctx, ws, name); err == nil {
			spec.Did = live.Spec.Did
			if spec.Did != "" {
				spec.PdsURL = live.Spec.PdsURL
			}
		}
	}
	if spec.Mode == v1alpha1.MarketIdentityModeLocalRepo {
		probe := RefLink(AppRef(KindMarketIdentity, name, ws), AppRef(ComponentKindPds, PdsComponent, r.Workspace),
			Cap(bus.CapabilityXRPC, "com.atproto.server.describeServer"))
		if err := c.ApplyLinks(ctx, r, "identity-"+name, []Link{probe}); err != nil {
			return err
		}
	}
	obj := &v1alpha1.MarketIdentity{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}
	return c.ApplyTyped(ctx, r, ws, MarketIdentityGVR, v1alpha1.GroupVersion.String(), KindMarketIdentity, obj)
}

func (c *Composer) ApplyOAuthIdentityEndpoints(ctx context.Context, r Root, snap Snapshot, ws, identity, exposure, role string) error {
	raw, _ := snap.ExposureURL(exposure)
	base := strings.TrimRight(raw, "/")
	if base == "" {
		return nil
	}
	id, err := c.Identity(ctx, ws, identity)
	if err != nil {
		return err
	}
	switch id.Spec.Mode {
	case v1alpha1.MarketIdentityModeOAuthBrowser, v1alpha1.MarketIdentityModeOAuthQr, v1alpha1.MarketIdentityModeOAuthDelegated:
	default:
		return nil
	}
	u := NewDoc(v1alpha1.GroupVersion.String(), KindMarketIdentity, identity)
	u["spec"] = map[string]any{"oauthClientID": base + "/oauth-client-metadata.json", "oauthRedirectURI": base + "/oauth/callback/" + role}
	return c.applyAs(ctx, r, r.IdentityManager(), ws, MarketIdentityGVR, u)
}

func ClaimOpen(id *v1alpha1.MarketIdentity) bool {
	if id == nil || strings.TrimSpace(id.Status.Did) == "" {
		return false
	}
	for _, cond := range id.Status.Conditions {
		if cond.Type == v1alpha1.ConditionAssociated && cond.Reason == "Revoked" {
			return false
		}
	}
	if id.Spec.Mode == v1alpha1.MarketIdentityModeLocalRepo {
		if id.Spec.Association == v1alpha1.AssociationSkip || AssociatedClaim(id) || len(id.Status.Associations) > 0 {
			return false
		}
		if id.Status.Phase == v1alpha1.MarketIdentityPhaseAwaitingApproval {
			return true
		}
		return id.Status.Phase == v1alpha1.MarketIdentityPhaseReady && len(id.Spec.ApprovedAssociations) == 0
	}
	return id.Status.Phase == v1alpha1.MarketIdentityPhaseAwaitingScan && id.Status.PendingAction != nil
}

func AssociatedClaim(id *v1alpha1.MarketIdentity) bool {
	if id == nil {
		return false
	}
	for _, cond := range id.Status.Conditions {
		if cond.Type == v1alpha1.ConditionAssociated && cond.Status == metav1.ConditionTrue {
			return true
		}
	}
	return false
}

func associationRequired(plan AccountPlan, id *v1alpha1.MarketIdentity) bool {
	if plan.Mode != v1alpha1.MarketIdentityModeLocalRepo || plan.Identity.Association == v1alpha1.AssociationSkip || id == nil {
		return false
	}
	return strings.TrimSpace(id.Spec.AssociationWatch) != "" || strings.TrimSpace(id.Spec.AssociateWith) != ""
}

func AssociationWait(plan AccountPlan, id *v1alpha1.MarketIdentity, service string) string {
	if !associationRequired(plan, id) || len(plan.Identity.ApprovedAssociations) > 0 {
		return ""
	}
	if id != nil && id.Status.Phase == v1alpha1.MarketIdentityPhaseReady && (AssociatedClaim(id) || len(id.Status.Associations) > 0) {
		return ""
	}
	keyID := "the requester DID"
	if id != nil {
		if did := strings.TrimSpace(id.Status.Did); did != "" {
			keyID = did
		}
	}
	return fmt.Sprintf("waiting for the operator's %s claim record (challenge=operator, keyId=%s, service=%s)", service, keyID, service)
}

func OperatorDID(declared []string, id *v1alpha1.MarketIdentity) string {
	for _, did := range declared {
		if did = strings.TrimSpace(did); did != "" {
			return did
		}
	}
	if id == nil {
		return ""
	}
	for _, did := range id.Status.Associations {
		if did = strings.TrimSpace(did); did != "" {
			return did
		}
	}
	for _, did := range id.Spec.ApprovedAssociations {
		if did = strings.TrimSpace(did); did != "" {
			return did
		}
	}
	return ""
}

func (c *Composer) ApplyRemotePDS(ctx context.Context, r Root, ws string, desired *pdsv1alpha1.RemotePDS) (*pdsv1alpha1.RemotePDS, error) {
	if desired == nil || strings.TrimSpace(desired.Name) == "" {
		return nil, nil
	}
	obj := &pdsv1alpha1.RemotePDS{ObjectMeta: metav1.ObjectMeta{Name: desired.Name}, Spec: desired.Spec}
	if err := c.ApplyTyped(ctx, r, ws, RemotePDSGVR, pdsv1alpha1.GroupVersion.String(), KindRemotePDS, obj); err != nil {
		return nil, err
	}
	return obj, nil
}

func (c *Composer) AccountRepo(ctx context.Context, r Root, ws, identityName string, plan AccountPlan, did, pdsURL string) ([]Link, error) {
	if plan.Local || strings.TrimSpace(did) == "" {
		return nil, nil
	}
	binding, err := plan.binding(did, pdsURL)
	if err != nil {
		return nil, err
	}
	if binding.RemotePDS != nil {
		ensured, err := c.ApplyRemotePDS(ctx, r, ws, binding.RemotePDS)
		if err != nil {
			return nil, err
		}
		if ensured != nil {
			binding.PDSRef = &commonv1.ObjectRef{Group: pdsv1alpha1.GroupName, Kind: KindRemotePDS, Name: ensured.Name}
		}
	}
	if !plan.UsesAssociation() {
		c.DeleteAssociationObjects(ctx, ws)
	}
	if err := c.PruneLocalAccounts(ctx, r, ws, LocalAccount{}); err != nil {
		return nil, err
	}
	name := atprotorepo.RepoName(binding.DID)
	identity := AppRef(KindMarketIdentity, identityName, ws)
	spec := v1alpha1.RemoteRepoSpec{DID: binding.DID, PdsRef: *binding.PDSRef,
		CredentialsRef: commonv1.CredentialsRef{ObjectRef: commonv1.ObjectRef{Group: v1alpha1.GroupName, Kind: KindMarketIdentity, Name: identityName}}}
	if err := c.ApplyRepo(ctx, r, ws, name, KindRemoteRepo, v1alpha1.RemoteRepoGVR, &v1alpha1.RemoteRepo{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: spec}); err != nil {
		return nil, err
	}
	remote := AppRef(KindRemoteRepo, name, ws)
	links := []Link{RepoBackendLink(AppRef(KindAtprotoRepo, name, ws), remote)}
	links = append(links, RemoteRepoLinks(remote, RemotePDSRefIn(binding.PDSRef.Name, ws), identity, binding.DID)...)
	if binding.OwnsAuth {
		links = append(links, RemotePDSAuthLinks(RemotePDSRefIn(binding.PDSRef.Name, ws), binding.RemotePDS.Spec.Auth)...)
	}
	return links, nil
}
