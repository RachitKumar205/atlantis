package provision

// Reading and replacing the container images one organisation runs.
//
// Ensure applies the configured images on every call, so an organisation built
// today carries whatever the provisioner was configured with today. Nothing
// revisits one built yesterday: reconcile checks that an organisation exists,
// not what it is running. Three organisations were found on three different
// server images while the control plane was several deploys ahead.

import (
	"context"
	"errors"
	"fmt"

	cnpgv1 "github.com/cloudnative-pg/cloudnative-pg/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

// ImageKind names one of the three images an organisation runs.
//
// A string so it can be stored in the request row and printed, and a closed set
// so a typo in a command does not silently roll nothing.
type ImageKind string

const (
	ImageServer   ImageKind = "server"
	ImageSigner   ImageKind = "signer"
	ImagePostgres ImageKind = "postgres"
)

// AllImageKinds is every kind, in the order a roll applies them.
//
// Postgres last. The other two restart in seconds behind a readiness probe;
// this one restarts the database, and an operator watching a roll should see
// the cheap changes land before the expensive one starts.
var AllImageKinds = []ImageKind{ImageServer, ImageSigner, ImagePostgres}

// ParseImageKind returns the kind named by s.
func ParseImageKind(s string) (ImageKind, error) {
	for _, k := range AllImageKinds {
		if string(k) == s {
			return k, nil
		}
	}
	return "", fmt.Errorf("provision: %q is not an image kind (server, signer, postgres)", s)
}

// Images are the images an organisation is running, read from the cluster.
//
// A missing field means the object holding it was absent, which Exists does not
// catch: it reads the namespace, and a namespace survives a Deployment somebody
// deleted.
type Images struct {
	Server   string
	Signer   string
	Postgres string
}

// Get returns the image for one kind.
func (i Images) Get(kind ImageKind) string {
	switch kind {
	case ImageServer:
		return i.Server
	case ImageSigner:
		return i.Signer
	case ImagePostgres:
		return i.Postgres
	}
	return ""
}

// Configured are the images this provisioner would apply to an organisation it
// built now.
func (k *Kube) Configured() Images {
	return Images{
		Server:   k.cfg.ServerImage,
		Signer:   k.cfg.SignerImage,
		Postgres: k.cfg.PostgresImage,
	}
}

// Images reads what one organisation is running.
//
// The pod template's image, not a running pod's: the template is what the next
// pod starts from, so a Deployment patched a moment ago reports the new image
// while the old pod is still terminating. A roll is finished when the template
// and the configuration agree; whether the rollout has completed is what the
// Deployment's own conditions say.
//
// An absent object leaves its field empty rather than failing the read. One
// organisation missing a signer should report as drift on that organisation,
// not stop the pass that would have reported the other fifty.
func (k *Kube) Images(ctx context.Context, org string) (Images, error) {
	if org == "" {
		return Images{}, errors.New("provision: an organisation name is required")
	}
	ns := k.cfg.Namespace(org)

	var out Images
	for _, d := range []struct {
		name string
		into *string
	}{
		{nameAtlantis, &out.Server},
		{nameSigner, &out.Signer},
	} {
		var dep appsv1.Deployment
		err := k.c.Get(ctx, ctrlclient.ObjectKey{Namespace: ns, Name: d.name}, &dep)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return Images{}, fmt.Errorf("read %s/%s: %w", ns, d.name, err)
		}
		if c := dep.Spec.Template.Spec.Containers; len(c) > 0 {
			*d.into = c[0].Image
		}
	}

	var cluster cnpgv1.Cluster
	err := k.c.Get(ctx, ctrlclient.ObjectKey{Namespace: ns, Name: namePostgres}, &cluster)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return Images{}, fmt.Errorf("read %s/%s: %w", ns, namePostgres, err)
	default:
		out.Postgres = cluster.Spec.ImageName
	}
	return out, nil
}

// RollImages replaces the images for the named kinds with the configured ones.
//
// The whole object is re-applied from the same builder Ensure uses, not a patch
// carrying only the image. Server-side apply drops fields this owner set and
// this apply omits, so a partial object under the same field owner would strip
// the rest of the spec — the probes, the resources, the volumes.
//
// Re-applying converges everything else the builder decides, which is the
// behaviour Ensure already has on every provisioning attempt.
//
// Postgres restarts. The Cluster sets no primaryUpdateStrategy, so CloudNativePG
// applies its default of unsupervised with an in-place restart, and the tenant
// runs one instance: changing imageName takes the database down for the length
// of a restart, measured at about 110 seconds. The other two are Deployments
// behind a readiness probe.
func (k *Kube) RollImages(ctx context.Context, org string, kinds []ImageKind) error {
	if org == "" {
		return errors.New("provision: an organisation name is required")
	}
	if len(kinds) == 0 {
		return errors.New("provision: no image kinds to roll")
	}
	ns := k.cfg.Namespace(org)

	for _, kind := range AllImageKinds {
		if !containsKind(kinds, kind) {
			continue
		}
		var obj ctrlclient.Object
		switch kind {
		case ImageServer:
			obj = k.atlantisDeployment(ns)
		case ImageSigner:
			obj = k.signerDeployment(ns)
		case ImagePostgres:
			obj = k.postgres(ns)
		}
		if err := k.apply(ctx, obj); err != nil {
			return fmt.Errorf("roll %s for %s: %w", kind, org, err)
		}
		k.log.Info("rolled an organisation's image",
			"org", org, "namespace", ns, "kind", kind, "image", k.Configured().Get(kind))
	}
	return nil
}

func containsKind(kinds []ImageKind, want ImageKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}
