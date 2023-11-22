package sharing_secret

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	secretController = "secret-controller"
)

type SecretReconciler struct {
	recorder record.EventRecorder
	logger   logr.Logger
	client.Client
}

func (r *SecretReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.Client = mgr.GetClient()
	r.logger = ctrl.Log.WithName(secretController)
	r.recorder = mgr.GetEventRecorderFor(secretController)

	labelSelectorPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key:      sharingSecretRef,
			Operator: metav1.LabelSelectorOpExists,
		}}})

	if err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named(secretController).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 2,
		}).
		For(&corev1.Secret{}).
		WithEventFilter(labelSelectorPredicate).
		Complete(r)
}

func (r *SecretReconciler) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, request.NamespacedName, secret); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if len(secret.OwnerReferences) != 1 {
		return reconcile.Result{}, nil
	}

	if secret.OwnerReferences[0].Kind != "SharingSecret" {
		return reconcile.Result{}, nil
	}

	finalizer := "experimental.kubesphere.io/cleanup"
	if secret.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted, so if it does not have our finalizer,
		// then lets add the finalizer and update the object.
		if !controllerutil.ContainsFinalizer(secret, finalizer) {
			expected := secret.DeepCopy()
			controllerutil.AddFinalizer(expected, finalizer)
			return ctrl.Result{}, r.Patch(ctx, expected, client.MergeFrom(secret))
		}
	} else {
		// The object is being deleted
		if controllerutil.ContainsFinalizer(secret, finalizer) {
			if err := r.cleanup(ctx, secret); err != nil {
				return ctrl.Result{}, fmt.Errorf("failed to delete related resources: %s", err)
			}
			// remove our finalizer from the list and update it.
			controllerutil.RemoveFinalizer(secret, finalizer)
			if err := r.Update(ctx, secret, &client.UpdateOptions{}); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, client.ObjectKey{Name: "default", Namespace: secret.Namespace}, sa); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	isDefaultImagePullSecret := secret.Annotations[defaultImagePullSecretAnnotation] == "true"

	inDefaultPullSecrets := false
	for _, item := range sa.ImagePullSecrets {
		if item.Name == secret.Name {
			inDefaultPullSecrets = true
		}
	}

	if isDefaultImagePullSecret && !inDefaultPullSecrets {
		sa.ImagePullSecrets = append(sa.ImagePullSecrets, corev1.LocalObjectReference{Name: secret.Name})
		if err := r.Update(ctx, sa, &client.UpdateOptions{}); err != nil {
			return ctrl.Result{}, err
		}
	}

	if !isDefaultImagePullSecret && inDefaultPullSecrets {
		for i, item := range sa.ImagePullSecrets {
			if item.Name == secret.Name {
				sa.ImagePullSecrets = append(sa.ImagePullSecrets[:i], sa.ImagePullSecrets[i+1:]...)
				if err := r.Update(ctx, sa, &client.UpdateOptions{}); err != nil {
					return ctrl.Result{}, err
				}
				break
			}
		}
	}

	r.logger.Info("default image pull secret successfully synced", "namespace", request.Namespace, "secret", secret.Name)
	return reconcile.Result{}, nil
}

func (r *SecretReconciler) cleanup(ctx context.Context, secret *corev1.Secret) error {
	sa := &corev1.ServiceAccount{}
	if err := r.Get(ctx, client.ObjectKey{Name: "default", Namespace: secret.Namespace}, sa); err != nil {
		return client.IgnoreNotFound(err)
	}
	for i, item := range sa.ImagePullSecrets {
		if item.Name == secret.Name {
			sa.ImagePullSecrets = append(sa.ImagePullSecrets[:i], sa.ImagePullSecrets[i+1:]...)
			return r.Update(ctx, sa, &client.UpdateOptions{})
		}
	}
	return nil
}
