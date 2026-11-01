package marketrequester

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	commonv1 "github.com/johnandersen777/socialweb-computer-kcp/api/common/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/api/v1alpha1"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/app/compose"
	ctlruntime "github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime"
	"github.com/johnandersen777/socialweb-computer-kcp/internal/controllers/runtime/manifests"
)

const (
	Name = "marketrequester"

	Kind = string(v1alpha1.ApplicationKindMarketRequester)

	RequesterSet            = "market-requester"
	RequesterComponent      = "requester"
	RequesterApp            = "market-requester"
	RequesterVaultServer    = "requester"
	RequesterVaultClient    = "requester"
	RequesterAccount        = "requester"
	RequesterIdentity       = "requester"
	RequesterContract       = "contract"
	RequesterProfileNetwork = "market-network"
	RequesterProfileDefault = "default"
)

var Workflows = []string{"requester-tangled-vouch", "requester-only-me", "requester-mutuals", "open"}

type Controller struct {
	c *compose.Composer
}

func Register(m *ctlruntime.Manager, c *compose.Composer) error {
	ctl := &Controller{c: c}
	c.ProvideTrust(schema.GroupKind{Group: v1alpha1.GroupName, Kind: Kind})
	opts := append([]ctlruntime.Option{ctlruntime.Export(compose.ExportApplications), ctlruntime.Finalizer(compose.Finalizer)}, c.Watches(Kind)...)
	if err := ctlruntime.For[*v1alpha1.MarketRequester](m, Name, ctl, opts...); err != nil {
		return fmt.Errorf("marketrequester: registering the MarketRequester controller: %w", err)
	}
	return nil
}

type pass struct {
	ctx context.Context

	ctl *Controller

	c *compose.Composer

	r compose.Root

	o *v1alpha1.MarketRequester

	st v1alpha1.ApplicationStatus

	links []compose.Link
}

func (p *pass) write(phase v1alpha1.ApplicationPhase, msg string) (ctlruntime.Result, error) {
	p.st.Phase, p.st.Message = phase, msg
	return ctlruntime.Result{}, p.c.WriteStatus(p.ctx, p.r, compose.MarketRequesterGVR, p.st)
}

func (p *pass) fail(err error) (ctlruntime.Result, error) {
	compose.SetOverrideErrorIfAny(&p.st.Conditions, &p.st.ComponentSummary, p.o.Generation, err)
	return p.write(v1alpha1.ApplicationPhaseFailed, err.Error())
}

func (p *pass) retry(what string, err error) (ctlruntime.Result, error) {
	msg := err.Error()
	if what != "" {
		msg = what + ": " + msg
	}
	if _, werr := p.write(v1alpha1.ApplicationPhaseProvisioning, msg); werr != nil {
		return ctlruntime.Result{}, errors.Join(err, werr)
	}
	return ctlruntime.Result{}, err
}

func (p *pass) wait(msg string) (ctlruntime.Result, error) {
	return p.write(v1alpha1.ApplicationPhaseProvisioning, msg)
}

func (p *pass) snapshot(ctx context.Context) (compose.Snapshot, error) {
	snap, err := p.c.Snapshot(ctx, p.r.Workspace, p.r.Owner())
	if err != nil {
		return snap, err
	}
	p.st.Components, p.st.Listeners = snap.Components, snap.Listeners
	return snap, nil
}

func (ctl *Controller) Reconcile(ctx context.Context, req ctlruntime.Request, o *v1alpha1.MarketRequester) (ctlruntime.Result, error) {
	c := ctl.c
	if !c.Mine(req) {
		return ctlruntime.Result{}, nil
	}
	r := compose.RootOf(req, Kind, o, o.Spec.Suspend)
	c.Observe(r)
	p := &pass{ctx: ctx, ctl: ctl, c: c, r: r, o: o, st: compose.NewStatus(o.Generation, o.Spec.Profile, o.Status)}
	if r.Deleting {
		return p.delete(ctx)
	}
	res, err := p.reconcile(ctx)
	if err == nil {
		c.Adopt(ctx, r)
		c.ApplyQuotas(ctx, r, o.Spec.Quota)
	}
	return res, err
}

func (p *pass) delete(ctx context.Context) (ctlruntime.Result, error) {
	left, err := p.c.StopComponents(ctx, p.r, "")
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if left == 0 {
		if left, err = p.c.DeleteVaults(ctx, p.r); err != nil {
			return ctlruntime.Result{}, err
		}
	}
	if left > 0 {
		p.st.Phase, p.st.Message = v1alpha1.ApplicationPhaseDeleting, fmt.Sprintf("%d component(s) stopping", left)
		return ctlruntime.Result{Waiting: true}, p.c.WriteStatus(ctx, p.r, compose.MarketRequesterGVR, p.st)
	}
	p.c.Drop(p.r)
	return ctlruntime.Result{}, nil
}

func (p *pass) self() commonv1.ObjectRef {
	return compose.AppRef(compose.ComponentKindRequester, RequesterComponent, p.r.Workspace)
}

func (p *pass) baseLinks(mount string) []compose.Link {
	self := p.self()
	return []compose.Link{
		compose.PolicyRunLink(self, compose.PolicyEngineRefIn(compose.PolicyEngineComponent, p.r.Workspace), Workflows...),
		compose.VaultLink(self, RequesterVaultServer, false, mount+"/*"),
		compose.VaultLink(self, RequesterVaultServer, true, mount+"/contracts/*"),
		compose.RootVaultLink(p.r, RequesterVaultServer, mount+"/identity/*"),
	}
}

func (p *pass) reconcile(ctx context.Context) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	ws := r.Workspace
	plan, err := c.PlanAccount(ctx, ws, o.Spec.Identity, "requester",
		compose.AccountVault{Server: RequesterVaultServer, Client: RequesterVaultClient, Mount: RequesterVaultClient},
		v1alpha1.AssociationRequireApproval)
	if err != nil {
		return p.fail(err)
	}
	// The identity controller reconciles before the app can patch the OAuth
	// endpoints into its status. Seed them from the app options so the first
	// oauthBrowser reconcile has a client ID and redirect URI.
	if plan.Mode == v1alpha1.MarketIdentityModeOAuthBrowser || plan.Mode == v1alpha1.MarketIdentityModeOAuthQr || plan.Mode == v1alpha1.MarketIdentityModeOAuthDelegated {
		for _, option := range o.Spec.Options {
			switch option.Name {
			case "oauth-client-id":
				plan.Identity.OAuthClientID = option.Value
			case "oauth-redirect-uri":
				plan.Identity.OAuthRedirectURI = option.Value
			}
		}
	}
	mount := plan.Vault.MountOr(RequesterVaultClient)
	pdsAccountHandle := compose.PdsHandle(ws, "requester", o.Spec.Identity.Handle)
	ingressSpec := compose.AppIngressOrDefault(o.Spec.Profile == RequesterProfileDefault, o.Spec.Ingress, o.Spec.Network)
	requireIngress, ingressRef, ingressConfig, ingressErr := compose.PdsAppIngress(ingressSpec, o.Spec.Network, o.Spec.Profile == RequesterProfileDefault)
	pdsPublicURL, _ := c.PdsPublicURL(ctx, ws, compose.PdsComponent, requireIngress)
	if r.Suspend {
		return p.suspended(ctx, plan, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef)
	}
	if err := c.ApplyVault(ctx, r, RequesterVaultServer); err != nil {
		return ctlruntime.Result{}, err
	}
	p.links = append(p.links, p.baseLinks(mount)...)
	if !c.Applied(r) {
		if _, err := p.write(v1alpha1.ApplicationPhaseProvisioning, "applying its schemas, its own OpenBaoServer and its workspace"); err != nil {
			return ctlruntime.Result{}, err
		}
		if err := c.ApplyInfra(ctx, r, RequesterSet, o.Spec.Profile, &manifests.MarketRequester{}); err != nil {
			return p.retry("", err)
		}
		if err := c.ApplyIdentitySpec(ctx, r, ws, RequesterIdentity, plan.Identity); err != nil {
			return p.retry("its MarketIdentity", err)
		}
		c.MarkApplied(r)
	}
	if err := c.ApplyLinks(ctx, r, "root", p.links); err != nil {
		return ctlruntime.Result{}, err
	}
	if _, _, err := c.VaultRefs(ctx, ws, RequesterVaultServer, RequesterVaultClient); err != nil {
		return p.retry("its vault", err)
	}
	if ok, msg := c.LinksAccepted(ctx, r, p.links); !ok {
		return p.wait(msg)
	}
	if err := c.SeedPdsAccountPassword(ctx, r, plan); err != nil {
		return p.retry("its account password", err)
	}
	id, err := c.Identity(ctx, ws, RequesterIdentity)
	if apierrors.IsNotFound(err) {
		c.Forget(r)
		return p.wait("waiting for its MarketIdentity")
	}
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if !plan.Local {
		return p.account(ctx, plan, id)
	}
	return p.local(ctx, plan, id, pdsAccountHandle, pdsPublicURL, ingressConfig, requireIngress, ingressRef, ingressErr)
}

func (p *pass) desired(ctx context.Context, plan compose.AccountPlan, id *v1alpha1.MarketIdentity, pdsHandle, pdsPublicURL string,
	ingress *v1alpha1.IngressConfig, requireIngress bool, ingressRef *commonv1.ObjectRef) ([]compose.Desired, error) {
	c, o, ws := p.c, p.o, p.r.Workspace
	stateRoot := c.StateRoot()
	network := o.Spec.Network
	ident := compose.ComponentIdentity(id, o.Spec.Identity.Handle)
	vault := compose.ComponentRefFor(compose.KindOpenBaoServer, RequesterVaultServer)
	var hostnames []string
	if ident.Handle != "" {
		hostnames = []string{ident.Handle}
	}
	mode, publicOrigin, oauthClientID, oauthRedirectURI, guestSSHKeysFile := compose.RequesterComponentFields(o.Spec.Options, plan.Mode)
	spec := v1alpha1.RequesterSpec{
		ComponentSpec:        p.c.NetworkComponentSpec(p.r, compose.ComponentCRStateDir(stateRoot, compose.ComponentKindRequester, RequesterComponent), network),
		Identity:             ident,
		PolicyEngineRef:      compose.ComponentRefFor(compose.ComponentKindPolicyEngine, compose.PolicyEngineComponent),
		VaultRef:             vault,
		Hostnames:            hostnames,
		Mode:                 mode,
		PublicOrigin:         publicOrigin,
		MarketExposureRef:    &commonv1.LocalObjectRef{Name: compose.ServiceNameFor(compose.ComponentKindRequester, RequesterComponent) + compose.ExposureIngressSuffix},
		OperatorDID:          compose.OperatorDID(o.Spec.Identity.ApprovedAssociations, id),
		OAuthClientID:        oauthClientID,
		OAuthRedirectURI:     oauthRedirectURI,
		GuestSSHKeysFile:     guestSSHKeysFile,
		BidderDIDs:           append([]string(nil), o.Spec.BidderDids...),
		AcceptCopies:         int32(compose.OptionInt(o.Spec.Options, "accept-copies")),
		Comms:                o.Spec.Comms,
		SuspendParticipation: o.Spec.Suspend,
	}
	out := []compose.Desired{compose.PolicyEngineDesired()}
	if plan.Local {
		pdsSpec, err := compose.AppPdsSpec(stateRoot, network, pdsHandle, vault, hostnames, pdsPublicURL, requireIngress, ingressRef)
		if err != nil {
			return nil, err
		}
		out = append(out, compose.Desired{Kind: compose.ComponentKindPds, Name: compose.PdsComponent, Spec: pdsSpec})
		spec.PdsRef = compose.ComponentRefFor(compose.ComponentKindPds, compose.PdsComponent)
	} else {
		spec.PdsRef = plan.PdsRef
		spec.PdsDID = plan.PdsDID
		spec.PDSAccessToken = plan.AccessToken()
	}
	out = append(out, compose.Desired{Kind: compose.ComponentKindRequester, Name: RequesterComponent, Spec: spec})
	out = append(out, compose.HTTPServiceExposures(ws, compose.ComponentKindRequester, RequesterComponent, "http", network, hostnames, false, true)...)
	if plan.Local {
		out = append(out, compose.PdsExposures(ws, compose.PdsComponent, network, hostnames, ingress)...)
	}
	return compose.ApplyOverrides(o.Spec.Components, out)
}

func (p *pass) ensure(ctx context.Context, desired []compose.Desired) error {
	return p.c.EnsureLinked(ctx, p.r, desired, desired)
}

func (p *pass) prune(ctx context.Context, desired []compose.Desired) error {
	_, err := p.c.DeleteComponentsExcept(ctx, p.r.Workspace, p.r.Owner(), compose.DesiredKeys(desired))
	return err
}

func (p *pass) suspended(ctx context.Context, plan compose.AccountPlan, pdsHandle, pdsPublicURL string,
	ingress *v1alpha1.IngressConfig, requireIngress bool, ingressRef *commonv1.ObjectRef) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	id, _ := c.Identity(ctx, r.Workspace, RequesterIdentity)
	desired, err := p.desired(ctx, plan, id, pdsHandle, pdsPublicURL, ingress, requireIngress, ingressRef)
	if err != nil {
		return p.fail(err)
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	held := compose.Existing(snap, desired)
	if err := p.ensure(ctx, held); err != nil {
		return p.retry("", err)
	}
	if snap, err = p.snapshot(ctx); err != nil {
		return ctlruntime.Result{}, err
	}
	if id != nil {
		p.st.Did = id.Status.Did
	}
	compose.FillApplicationStatus(&p.st, o.Generation, true, o.Spec.Components, held, snap, nil)
	p.st = compose.SuspendedOnceApplied(p.st, snap, held)
	return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.MarketRequesterGVR, p.st)
}

func (p *pass) streams(ctx context.Context, plan compose.AccountPlan, localDID string) error {
	links, err := p.c.ApplyMarketStreams(ctx, p.r, compose.Streams{Role: "requester", Comms: compose.OverrideComms(p.o.Spec.Components, compose.ComponentKindRequester, RequesterComponent, p.o.Spec.Comms),
		Sources: compose.SourceInput{Network: p.o.Spec.Network, LocalDID: localDID}, ForceFirehose: compose.OAuthReceiveOnly(plan.Mode)})
	if err != nil {
		return err
	}
	return p.c.ApplyLinks(ctx, p.r, "events", links)
}

func (p *pass) account(ctx context.Context, plan compose.AccountPlan, id *v1alpha1.MarketIdentity) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	ws := r.Workspace
	switch id.Status.Phase {
	case v1alpha1.MarketIdentityPhaseFailed, v1alpha1.MarketIdentityPhaseSessionExpired:
		return p.write(v1alpha1.ApplicationPhaseFailed, "its MarketIdentity: "+id.Status.Message)
	case v1alpha1.MarketIdentityPhaseReady:
	default:
		return p.wait("waiting for its existing-account MarketIdentity to sign in")
	}
	if strings.TrimSpace(id.Status.Did) == "" {
		return p.wait("waiting for its existing-account MarketIdentity to report a did")
	}
	if plan.Mode == v1alpha1.MarketIdentityModePdsAccount && strings.TrimSpace(id.Status.PdsURL) == "" {
		return p.write(v1alpha1.ApplicationPhaseFailed, "the existing-account MarketIdentity reports no pdsUrl; it fails closed")
	}
	desired, err := p.desired(ctx, plan, id, "", "", nil, false, nil)
	if err != nil {
		return p.fail(err)
	}
	if err := p.ensure(ctx, desired); err != nil {
		return p.retry("", err)
	}
	if err := p.streams(ctx, plan, ""); err != nil {
		return p.fail(err)
	}
	if err := p.prune(ctx, desired); err != nil {
		return ctlruntime.Result{}, err
	}
	repoLinks, err := c.AccountRepo(ctx, r, ws, RequesterIdentity, plan, id.Status.Did, id.Status.PdsURL)
	if err != nil {
		c.Log().Warn("the existing-account repo", "root", r.Owner(), "err", err.Error())
	}
	repoLinks = append(repoLinks, compose.RepoWriteLink(p.self(), compose.RepoRefIn(id.Status.Did, ws), compose.ScopeAll))
	if err := c.ApplyLinks(ctx, r, "repo", repoLinks); err != nil {
		return ctlruntime.Result{}, err
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if err := c.ApplyOAuthIdentityEndpoints(ctx, r, snap, ws, RequesterIdentity, compose.ServiceNameFor(compose.ComponentKindRequester, RequesterComponent)+compose.ExposureIngressSuffix, "requester"); err != nil {
		return ctlruntime.Result{}, err
	}
	p.st.Did = id.Status.Did
	compose.FillApplicationStatus(&p.st, o.Generation, false, o.Spec.Components, desired, snap, nil)
	if snap.Satisfied(desired) {
		p.st = compose.Ready(p.st, "the existing account is signed in and every component is ready")
		return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.MarketRequesterGVR, p.st)
	}
	return p.wait("waiting for every component to be ready")
}

func (p *pass) local(ctx context.Context, plan compose.AccountPlan, id *v1alpha1.MarketIdentity, pdsHandle, pdsPublicURL string,
	ingress *v1alpha1.IngressConfig, requireIngress bool, ingressRef *commonv1.ObjectRef, ingressErr error) (ctlruntime.Result, error) {
	c, r, o := p.c, p.r, p.o
	ws := r.Workspace
	desired, err := p.desired(ctx, plan, id, pdsHandle, pdsPublicURL, ingress, requireIngress, ingressRef)
	if err != nil {
		return p.fail(err)
	}
	first := compose.PdsApplySet(desired, compose.PdsComponent, compose.ComponentRefName(ingressRef))
	if requireIngress && pdsPublicURL == "" {
		if applied, err := c.PdsPublicURLApplied(ctx, ws, compose.PdsComponent); err != nil {
			return ctlruntime.Result{}, err
		} else if applied {
			first = compose.WithoutPds(first)
		}
	}
	if err := p.c.EnsureAdding(ctx, p.r, first); err != nil {
		return p.retry("", err)
	}
	snap, err := p.snapshot(ctx)
	if err != nil {
		return ctlruntime.Result{}, err
	}
	if !snap.Ready(compose.ComponentKindPds, compose.PdsComponent) {
		return p.wait("waiting for the requester's app Pds to be ready")
	}
	if requireIngress {
		if ingressErr != nil {
			compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", true, ingressErr)
			return p.wait("the app Pds ingress: " + ingressErr.Error())
		}
		if err := snap.PdsIngressReady(compose.PdsComponent, ingressRef); err != nil {
			compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", true, err)
			return p.wait("waiting for the app Pds ingress: " + err.Error())
		}
		if pdsPublicURL, err = c.PdsIngressURL(ctx, ws, compose.PdsComponent); err != nil {
			compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", true, err)
			return p.write(compose.PdsIngressPhase(err), "the app Pds ingress: "+err.Error())
		}
		compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", true, nil)
	} else {
		pdsPublicURL, _ = c.PdsPublicURL(ctx, ws, compose.PdsComponent, false)
		compose.SetPdsIngressCondition(&p.st.Conditions, o.Generation, "", false, nil)
	}
	if pdsPublicURL == "" {
		return p.wait("waiting for the app Pds public URL")
	}
	if desired, err = p.desired(ctx, plan, id, pdsHandle, pdsPublicURL, ingress, requireIngress, ingressRef); err != nil {
		return p.fail(err)
	}
	pds, _ := compose.DesiredPds(desired)
	if err := p.c.EnsureAdding(ctx, p.r, []compose.Desired{pds}); err != nil {
		return p.retry("", err)
	}
	if snap, err = p.snapshot(ctx); err != nil {
		return ctlruntime.Result{}, err
	}
	if !snap.PdsApplied(compose.PdsComponent) {
		return p.wait("waiting for the app Pds to apply its public URL")
	}
	account := compose.PdsAccount{Vault: RequesterVaultServer, Mount: plan.Vault.MountOr(RequesterVaultClient), Path: compose.PdsSessionPath("requester"), Handle: pdsHandle}
	if err := c.ApplyLinks(ctx, r, "account", compose.PdsAccountLinks(r, account)); err != nil {
		return ctlruntime.Result{}, err
	}
	if ok, msg := c.LinksAccepted(ctx, r, compose.PdsAccountLinks(r, account)); !ok {
		return p.wait(msg)
	}
	did, err := c.EnsurePdsAccount(ctx, r, account)
	if err != nil {
		return p.retry("its app Pds account", err)
	}
	acct := compose.LocalAccount{DID: did, Handle: pdsHandle, SessionID: RequesterVaultServer + ":" + account.Path, PLC: o.Spec.Network != nil}
	repoLinks := append(compose.LocalAccountLinks(ws, acct), compose.RepoWriteLink(p.self(), compose.RepoRefIn(did, ws), compose.ScopeAll),
		compose.RecordWriteLink(ws, r.Owner(), compose.RepoRefIn(did, ws), compose.ScopeAll))
	if err := c.ApplyLinks(ctx, r, "repo", repoLinks); err != nil {
		return ctlruntime.Result{}, err
	}
	if err := c.ApplyLocalAccount(ctx, r, ws, acct); err != nil {
		return ctlruntime.Result{}, err
	}
	requesterDir := compose.ComponentCRStateDir(c.StateRoot(), compose.ComponentKindRequester, RequesterComponent)
	changed, err := compose.SyncRequesterRepoKey(c.StateRoot(), did, requesterDir)
	if err != nil {
		return p.retry("its app Pds signing key", err)
	}
	if changed {
		if err := c.RestartComponent(ctx, r, ws, compose.ComponentKindRequester, RequesterComponent); err != nil && !apierrors.IsNotFound(err) {
			return p.retry("restarting the requester", err)
		}
	}
	identity := plan.Identity
	identity.Did, identity.PdsURL, identity.Handle = did, pdsPublicURL, pdsHandle
	if err := c.ApplyIdentitySpec(ctx, r, ws, RequesterIdentity, identity); err != nil {
		return p.retry("its app Pds identity", err)
	}
	if id, err = c.Identity(ctx, ws, RequesterIdentity); err != nil {
		return ctlruntime.Result{}, err
	}
	if desired, err = p.desired(ctx, plan, id, pdsHandle, pdsPublicURL, ingress, requireIngress, ingressRef); err != nil {
		return p.fail(err)
	}
	if err := p.c.EnsureLinked(ctx, p.r, desired, compose.WithoutPds(desired)); err != nil {
		return p.retry("", err)
	}
	if err := p.streams(ctx, plan, did); err != nil {
		return p.fail(err)
	}
	if err := p.prune(ctx, desired); err != nil {
		return ctlruntime.Result{}, err
	}
	if snap, err = p.snapshot(ctx); err != nil {
		return ctlruntime.Result{}, err
	}
	p.st.Did = id.Status.Did
	compose.FillApplicationStatus(&p.st, o.Generation, false, o.Spec.Components, desired, snap, nil)
	switch msg := compose.AssociationWait(plan, id, compose.RequesterAssociate); {
	case msg != "":
		return p.wait(msg)
	case snap.Satisfied(desired) && p.st.Did != "":
		p.st = compose.Ready(p.st, "the requester is signed in and its components are ready")
		return ctlruntime.Result{}, c.WriteStatus(ctx, r, compose.MarketRequesterGVR, p.st)
	case p.st.Did == "":
		return p.wait("waiting for the requester's MarketIdentity to sign in")
	}
	return p.wait("waiting for every component to be ready")
}
