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

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	prometheusv1 "github.com/coreos/prometheus-operator/pkg/apis/monitoring/v1"
	"github.com/go-logr/logr"
	objectbucket "github.com/kube-object-storage/lib-bucket-provisioner/pkg/apis/objectbucket.io/v1alpha1"
	routev1 "github.com/openshift/api/route/v1"
	"github.com/tidwall/sjson"
	"gopkg.in/yaml.v2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	quayredhatcomv1 "github.com/quay/quay-operator/apis/quay/v1"
	v1 "github.com/quay/quay-operator/apis/quay/v1"
	quaycontext "github.com/quay/quay-operator/pkg/context"
	"github.com/quay/quay-operator/pkg/kustomize"
)

const (
	upgradePollInterval  = time.Second * 10
	upgradePollTimeout   = time.Second * 6000
	creationPollInterval = time.Second * 1
	creationPollTimeout  = time.Second * 600

	GrafanaDashboardConfigMapNameSuffix = "grafana-dashboard-quay"
	GrafanaTitleJSONPath                = "title"
	GrafanaNamespaceFilterJSONPath      = "templating.list.1.options.0.value"
	GrafanaServiceFilterJSONPath        = "templating.list.2.options.0.value"
	ClusterMonitoringLabelKey           = "openshift.io/cluster-monitoring"
	QuayDashboardJSONKey                = "quay.json"
	QuayOperatorManagedLabelKey         = "quay-operator/managed-label"
	QuayOperatorFinalizer               = "quay-operator/finalizer"
)

// QuayRegistryReconciler reconciles a QuayRegistry object
type QuayRegistryReconciler struct {
	client.Client
	Log           logr.Logger
	Scheme        *runtime.Scheme
	EventRecorder record.EventRecorder
}

// +kubebuilder:rbac:groups=quay.redhat.com,resources=quayregistries,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=quay.redhat.com,resources=quayregistries/status,verbs=get;update;patch

func (r *QuayRegistryReconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	log := r.Log.WithValues("quayregistry", req.NamespacedName)

	log.Info("begin reconcile")

	var quay v1.QuayRegistry
	if err := r.Client.Get(ctx, req.NamespacedName, &quay); err != nil {
		if errors.IsNotFound(err) {
			log.Info("`QuayRegistry` deleted")
		} else {
			log.Error(err, "unable to retrieve QuayRegistry")
		}

		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	updatedQuay := quay.DeepCopy()
	quayContext := quaycontext.NewQuayRegistryContext()

	isQuayMarkedToBeDeleted := quay.GetDeletionTimestamp() != nil
	if isQuayMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(updatedQuay, QuayOperatorFinalizer) {
			if err := r.finalizeQuay(ctx, updatedQuay); err != nil {
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(updatedQuay, QuayOperatorFinalizer)
			err := r.Update(ctx, updatedQuay)
			if err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if available := v1.GetCondition(quay.Status.Conditions, v1.ConditionTypeAvailable); available != nil && available.Reason == v1.ConditionReasonMigrationsInProgress {
		log.Info("migrations in progress, skipping reconcile")

		return ctrl.Result{}, nil
	}

	if !v1.CanUpgrade(quay.Status.CurrentVersion) {
		err := fmt.Errorf("cannot upgrade %s => %s", quay.Status.CurrentVersion, v1.QuayVersionCurrent)

		return r.reconcileWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonUpgradeUnsupported, err.Error())
	}

	if quay.Spec.ConfigBundleSecret == "" {
		log.Info("`spec.configBundleSecret` is unset. Creating base `Secret`")

		baseConfigBundle, err := v1.EnsureOwnerReference(&quay, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: quay.GetName() + "-config-bundle-",
				Namespace:    quay.GetNamespace(),
			},
			Data: map[string][]byte{
				"config.yaml": encode(kustomize.BaseConfig()),
			},
		})
		if err != nil {
			msg := fmt.Sprintf("unable to add owner reference to base config bundle `Secret`: %s", err)

			return r.reconcileWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonConfigInvalid, msg)
		}

		if err := r.Client.Create(ctx, baseConfigBundle); err != nil {
			msg := fmt.Sprintf("unable to create base config bundle `Secret`: %s", err)

			return r.reconcileWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonConfigInvalid, msg)
		}

		objectMeta, _ := meta.Accessor(baseConfigBundle)
		updatedQuay.Spec.ConfigBundleSecret = objectMeta.GetName()
		if err := r.Client.Update(ctx, updatedQuay); err != nil {
			log.Error(err, "unable to update `spec.configBundleSecret`")
			return ctrl.Result{}, nil
		}

		log.Info("successfully updated `spec.configBundleSecret`")
		return ctrl.Result{}, nil
	}

	var configBundle corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: quay.GetNamespace(), Name: quay.Spec.ConfigBundleSecret}, &configBundle); err != nil {
		msg := fmt.Sprintf("unable to retrieve referenced `configBundleSecret`: %s, error: %s", quay.Spec.ConfigBundleSecret, err)

		return r.reconcileWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonConfigInvalid, msg)
	}

	log.Info("successfully retrieved referenced `configBundleSecret`", "configBundleSecret", configBundle.GetName(), "resourceVersion", configBundle.GetResourceVersion())

	quayContext, updatedQuay, err := r.checkManagedKeys(quayContext, updatedQuay.DeepCopy(), configBundle.Data["config.yaml"])
	if err != nil {
		msg := fmt.Sprintf("unable to retrieve managed keys `Secret`: %s", err)

		return r.reconcileWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonConfigInvalid, msg)
	}

	quayContext, updatedQuay, err = r.checkRoutesAvailable(quayContext, updatedQuay.DeepCopy(), configBundle.Data["config.yaml"])
	if err != nil && v1.ComponentIsManaged(updatedQuay.Spec.Components, v1.ComponentRoute) {
		msg := fmt.Sprintf("could not check for `Routes` API: %s", err)

		return r.reconcileWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonRouteComponentDependencyError, msg)
	}

	quayContext, updatedQuay, err = r.checkObjectBucketClaimsAvailable(quayContext, updatedQuay.DeepCopy(), configBundle.Data["config.yaml"])
	if err != nil && v1.ComponentIsManaged(updatedQuay.Spec.Components, v1.ComponentObjectStorage) {
		msg := fmt.Sprintf("could not check for `ObjectBucketClaims` API: %s", err)
		if _, err = r.updateWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonObjectStorageComponentDependencyError, msg); err != nil {
			log.Error(err, "failed to update `conditions` of `QuayRegistry`")
		}

		return ctrl.Result{RequeueAfter: time.Millisecond * 1000}, nil
	}

	quayContext, updatedQuay, err = r.checkBuildManagerAvailable(quayContext, updatedQuay.DeepCopy(), configBundle.Data["config.yaml"])
	if err != nil {
		msg := fmt.Sprintf("could not check for build manager support: %s", err)

		return r.reconcileWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonObjectStorageComponentDependencyError, msg)
	}

	quayContext, updatedQuay, err = r.checkMonitoringAvailable(quayContext, updatedQuay.DeepCopy(), configBundle.Data["config.yaml"])
	if err != nil && v1.ComponentIsManaged(updatedQuay.Spec.Components, v1.ComponentMonitoring) {
		msg := fmt.Sprintf("could not check for monitoring support: %s", err)

		return r.reconcileWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonMonitoringComponentDependencyError, msg)
	}

	updatedQuay, err = v1.EnsureDefaultComponents(quayContext, updatedQuay.DeepCopy())
	if err != nil {
		log.Error(err, "could not ensure default `spec.components`")

		return ctrl.Result{}, nil
	}

	if !v1.ComponentsMatch(quay.Spec.Components, updatedQuay.Spec.Components) {
		log.Info("updating QuayRegistry `spec.components` to include defaults")
		if err = r.Client.Update(ctx, updatedQuay); err != nil {
			log.Error(err, "failed to update `spec.components` to include defaults")

			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, nil
	}

	var userProvidedConfig map[string]interface{}
	err = yaml.Unmarshal(configBundle.Data["config.yaml"], &userProvidedConfig)
	if err != nil {
		updatedQuay, err = r.updateWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonConfigInvalid, err.Error())
		if err != nil {
			log.Error(err, "failed to update `conditions` of `QuayRegistry`")

			return ctrl.Result{}, nil
		}
	}

	updatedQuay.Status.Conditions = v1.RemoveCondition(updatedQuay.Status.Conditions, v1.ConditionTypeRolloutBlocked)

	for _, component := range updatedQuay.Spec.Components {
		contains, err := kustomize.ContainsComponentConfig(userProvidedConfig, component.Kind)
		if err != nil {
			updatedQuay, err = r.updateWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonConfigInvalid, err.Error())
			if err != nil {
				log.Error(err, "failed to update `conditions` of `QuayRegistry`")

				return ctrl.Result{}, nil
			}
		}

		if component.Managed && contains {
			msg := fmt.Sprintf("%s component marked as managed, but `configBundleSecret` contains required fields", component.Kind)

			updatedQuay, err = r.updateWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonConfigInvalid, msg)
			if err != nil {
				log.Error(err, "failed to update `conditions` of `QuayRegistry`")

				return ctrl.Result{}, nil
			}
		} else if !component.Managed && v1.RequiredComponent(component.Kind) && !contains {
			msg := fmt.Sprintf("required component `%s` marked as unmanaged, but `configBundleSecret` is missing necessary fields", component.Kind)

			updatedQuay, err = r.updateWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonConfigInvalid, msg)
			if err != nil {
				log.Error(err, "failed to update `conditions` of `QuayRegistry`")

				return ctrl.Result{}, nil
			}
		}
	}

	log.Info("inflating QuayRegistry into Kubernetes objects using Kustomize")
	deploymentObjects, err := kustomize.Inflate(quayContext, updatedQuay, &configBundle, log)
	if err != nil {
		log.Error(err, "could not inflate QuayRegistry into Kubernetes objects")

		return ctrl.Result{}, nil
	}

	for _, obj := range deploymentObjects {
		// For metrics and dashboards to work, we need to deploy the Grafana ConfigMap
		// in the `openshift-config-managed` namespace and add the label
		// `openshift.io/cluster-monitoring: true` to the registry namespace
		if quayContext.SupportsMonitoring && isGrafanaConfigMap(obj) {
			obj = updateResourceNamespace(obj, GrafanaDashboardConfigNamespace)

			if obj, err = updateGrafanaDashboardData(obj, updatedQuay.GetName(), updatedQuay.GetNamespace()); err != nil {
				msg := fmt.Sprintf("Unable to update title on Grafana ConfigMap %s", err)
				return r.reconcileWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonMonitoringComponentDependencyError, msg)
			}
		}

		if err := r.createOrUpdateObject(ctx, obj, quay); err != nil {
			msg := fmt.Sprintf("all Kubernetes objects not created/updated successfully: %s", err)

			return r.reconcileWithCondition(&quay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonComponentCreationFailed, msg)
		}
	}

	if quayContext.SupportsMonitoring {
		err := r.patchNamespaceForMonitoring(ctx, quay)
		if err != nil {
			return r.reconcileWithCondition(updatedQuay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue,
				v1.ConditionReasonMonitoringComponentDependencyError, err.Error())
		}
	}

	updatedQuay, _ = v1.EnsureConfigEditorEndpoint(quayContext, updatedQuay)
	updatedQuay.Status.ConfigEditorCredentialsSecret = configEditorCredentialsSecretFrom(deploymentObjects)

	if c := v1.GetCondition(updatedQuay.Status.Conditions, v1.ConditionTypeRolloutBlocked); c != nil && c.Status == metav1.ConditionTrue && c.Reason == v1.ConditionReasonConfigInvalid {
		return r.reconcileWithCondition(updatedQuay, v1.ConditionTypeRolloutBlocked, metav1.ConditionTrue, v1.ConditionReasonConfigInvalid, c.Message)
	}

	updatedQuay, err = r.updateWithCondition(updatedQuay, v1.ConditionTypeRolloutBlocked, metav1.ConditionFalse, v1.ConditionReasonComponentsCreationSuccess, "all objects created/updated successfully")
	if err != nil {
		log.Error(err, "failed to update `conditions` of `QuayRegistry`")

		return ctrl.Result{}, nil
	}

	if !quayContext.ObjectStorageInitialized && v1.ComponentIsManaged(updatedQuay.Spec.Components, "objectstorage") {
		r.Log.Info("requeuing to populate values for managed component: `objectstorage`")

		return ctrl.Result{Requeue: true}, nil
	}

	if updatedQuay.Status.CurrentVersion != v1.QuayVersionCurrent {
		updatedQuay, err = r.updateWithCondition(updatedQuay, v1.ConditionTypeAvailable, metav1.ConditionFalse, v1.ConditionReasonMigrationsInProgress, "running database migrations")
		if err != nil {
			log.Error(err, "failed to update `conditions` of `QuayRegistry`")

			return ctrl.Result{}, nil
		}

		go func(quayRegistry *v1.QuayRegistry) {
			err = wait.Poll(upgradePollInterval, upgradePollTimeout, func() (bool, error) {
				log.Info("checking Quay upgrade deployment readiness")

				var upgradeDeployment appsv1.Deployment
				err = r.Client.Get(ctx, types.NamespacedName{Name: quayRegistry.GetName() + "-quay-app-upgrade", Namespace: quayRegistry.GetNamespace()}, &upgradeDeployment)
				if err != nil {
					log.Error(err, "could not retrieve Quay upgrade deployment during upgrade")

					return false, err
				}

				if upgradeDeployment.Spec.Size() < 1 {
					log.Info("upgrade deployment scaled down, skipping check")

					return true, nil
				}

				if upgradeDeployment.Status.ReadyReplicas > 0 {
					log.Info("Quay upgrade complete, updating `status.currentVersion`")

					updatedQuay, _ := v1.EnsureRegistryEndpoint(quayContext, updatedQuay, userProvidedConfig)
					msg := "all registry component healthchecks passing"
					condition := v1.Condition{
						Type:               v1.ConditionTypeAvailable,
						Status:             metav1.ConditionTrue,
						Reason:             v1.ConditionReasonHealthChecksPassing,
						Message:            msg,
						LastUpdateTime:     metav1.Now(),
						LastTransitionTime: metav1.Now(),
					}
					updatedQuay.Status.Conditions = v1.SetCondition(updatedQuay.Status.Conditions, condition)
					updatedQuay.Status.CurrentVersion = v1.QuayVersionCurrent
					r.EventRecorder.Event(updatedQuay, corev1.EventTypeNormal, string(v1.ConditionReasonHealthChecksPassing), msg)

					if err = r.Client.Status().Update(ctx, updatedQuay); err != nil {
						log.Error(err, "could not update QuayRegistry status with current version")

						return true, err
					}

					updatedQuay.Spec.Components = v1.EnsureComponents(updatedQuay.Spec.Components)
					if err = r.Client.Update(ctx, updatedQuay); err != nil {
						log.Error(err, "could not update QuayRegistry spec to complete upgrade")

						return true, err
					}

					log.Info("successfully updated `status` after Quay upgrade")

					return true, nil
				}
				return false, nil
			})

			if err != nil {
				log.Error(err, "Quay upgrade deployment never reached ready phase")
			}
		}(updatedQuay.DeepCopy())
	}

	// Add finalizer for this CR
	if !controllerutil.ContainsFinalizer(updatedQuay, QuayOperatorFinalizer) {
		controllerutil.AddFinalizer(updatedQuay, QuayOperatorFinalizer)
		err = r.Update(ctx, updatedQuay)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

// updateGrafanaDashboardData parses the Grafana Dashboard ConfigMap and updates the title and labels to filter the query by
func updateGrafanaDashboardData(obj k8sruntime.Object, quayName string, quayNamespace string) (k8sruntime.Object, error) {
	updatedObj := obj.DeepCopyObject()
	configMapObj := updatedObj.(*corev1.ConfigMap)

	dashboardConfigJSON := configMapObj.Data[QuayDashboardJSONKey]

	newTitle := fmt.Sprintf("Quay - %s - %s", quayNamespace, quayName)
	dashboardConfigJSON, err := sjson.Set(dashboardConfigJSON, GrafanaTitleJSONPath, newTitle)
	if err != nil {
		return nil, err
	}

	dashboardConfigJSON, err = sjson.Set(dashboardConfigJSON, GrafanaNamespaceFilterJSONPath, quayNamespace)
	if err != nil {
		return nil, err
	}

	metricsServiceName := fmt.Sprintf("%s-quay-metrics", quayName)
	dashboardConfigJSON, err = sjson.Set(dashboardConfigJSON, GrafanaServiceFilterJSONPath, metricsServiceName)
	if err != nil {
		return nil, err
	}

	configMapObj.Data[QuayDashboardJSONKey] = dashboardConfigJSON
	return configMapObj, nil
}

// updateResourceNamespace updates an Object's namespace replacing the existing namespace
func updateResourceNamespace(obj k8sruntime.Object, newNamespace string) k8sruntime.Object {
	updatedObj := obj.DeepCopyObject()
	updatedObj.(*corev1.ConfigMap).SetNamespace(newNamespace)
	return updatedObj
}

// isGrafanaConfigMap checks if an Object is the Grafana ConfigMap used in the monitoring component
func isGrafanaConfigMap(obj k8sruntime.Object) bool {
	configMapGVK := schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"}

	return configMapGVK == obj.GetObjectKind().GroupVersionKind() &&
		strings.HasSuffix(obj.(*corev1.ConfigMap).GetName(), GrafanaDashboardConfigMapNameSuffix)
}

func encode(value interface{}) []byte {
	yamlified, _ := yaml.Marshal(value)

	return yamlified
}

func decode(bytes []byte) interface{} {
	var value interface{}
	_ = yaml.Unmarshal(bytes, &value)

	return value
}

func (r *QuayRegistryReconciler) createOrUpdateObject(ctx context.Context, obj k8sruntime.Object, quay v1.QuayRegistry) error {
	objectMeta, _ := meta.Accessor(obj)
	groupVersionKind := obj.GetObjectKind().GroupVersionKind().String()

	immutableResources := map[string]bool{
		schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"}.String(): true,
	}

	log := r.Log.WithValues(
		"quayregistry", quay.GetNamespace(),
		"Name", objectMeta.GetName(), "GroupVersionKind", groupVersionKind)
	log.Info("creating/updating object")

	obj, err := v1.EnsureOwnerReference(&quay, obj)
	if err != nil {
		log.Error(err, "could not ensure `ownerReferences` before creating object", objectMeta.GetName(), "GroupVersionKind", groupVersionKind)

		return err
	}

	// managedFields cannot be set on a PATCH.
	objectMeta.SetManagedFields([]metav1.ManagedFieldsEntry{})

	if immutableResources[groupVersionKind] {
		log.Info("(re)creating immutable resource")

		propagationPolicy := metav1.DeletePropagationForeground
		if err := r.Client.Delete(ctx, obj, &client.DeleteOptions{PropagationPolicy: &propagationPolicy}); err != nil && !errors.IsNotFound(err) && !errors.IsAlreadyExists(err) {
			log.Error(err, "failed to delete immutable resource")

			return err
		}

		err := wait.Poll(creationPollInterval, creationPollTimeout, func() (bool, error) {
			if err := r.Client.Create(ctx, obj); err == nil {
				return true, nil
			} else if errors.IsAlreadyExists(err) {
				return false, nil
			} else {
				return false, err
			}
		})

		if err != nil {
			log.Error(err, "failed to create immutable resource")

			return err
		}

		log.Info("succefully (re)created immutable resource")
	} else {
		opts := []client.PatchOption{client.ForceOwnership, client.FieldOwner("quay-operator")}
		if err := r.Client.Patch(ctx, obj, client.Apply, opts...); err != nil {
			log.Error(err, "failed to create/update object")

			return err
		}
	}

	log.Info("finished creating/updating object")

	return nil
}

func (r *QuayRegistryReconciler) updateWithCondition(q *v1.QuayRegistry, t v1.ConditionType, s metav1.ConditionStatus, reason v1.ConditionReason, msg string) (*v1.QuayRegistry, error) {
	updatedQuay := q.DeepCopy()

	condition := v1.Condition{
		Type:               t,
		Status:             s,
		Reason:             reason,
		Message:            msg,
		LastUpdateTime:     metav1.Now(),
		LastTransitionTime: metav1.Now(),
	}
	updatedQuay.Status.Conditions = v1.SetCondition(q.Status.Conditions, condition)
	updatedQuay.Status.LastUpdate = time.Now().UTC().String()

	eventType := corev1.EventTypeNormal
	if s == metav1.ConditionTrue {
		eventType = corev1.EventTypeWarning
	}

	// FIXME: Need to pause here because race condition between updating `conditions` multiple times changes `resourceVersion`...
	time.Sleep(1000 * time.Millisecond)

	// Fetch first to ensure we have the right `resourceVersion` for updates.
	var currentQuay v1.QuayRegistry
	if err := r.Client.Get(context.Background(), types.NamespacedName{Namespace: q.GetNamespace(), Name: q.GetName()}, &currentQuay); err != nil {
		return nil, err
	}
	updatedQuay.SetResourceVersion(currentQuay.GetResourceVersion())

	if err := r.Client.Status().Update(context.Background(), updatedQuay); err != nil {
		return nil, err
	}
	// FIXME: Events are not being recorded during testing, making it hard to debug...
	r.EventRecorder.Event(updatedQuay, eventType, string(reason), msg)

	return updatedQuay, nil
}

// reconcileWithCondition sets the given condition on the `QuayRegistry` and returns a reconcile result.
func (r *QuayRegistryReconciler) reconcileWithCondition(q *v1.QuayRegistry, t v1.ConditionType, s metav1.ConditionStatus, reason v1.ConditionReason, msg string) (ctrl.Result, error) {
	_, err := r.updateWithCondition(q, t, s, reason, msg)

	return ctrl.Result{}, err
}

// SetupWithManager initializes the controller manager
func (r *QuayRegistryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// FIXME: Can we do this in the `init()` function in `main.go`...?
	if err := routev1.AddToScheme(mgr.GetScheme()); err != nil {
		r.Log.Error(err, "Failed to add OpenShift `Route` API to scheme")

		return err
	}
	// FIXME: Can we do this in the `init()` function in `main.go`...?
	if err := objectbucket.AddToScheme(mgr.GetScheme()); err != nil {
		r.Log.Error(err, "Failed to add `ObjectBucketClaim` API to scheme")

		return err
	}

	if err := prometheusv1.AddToScheme(mgr.GetScheme()); err != nil {
		r.Log.Error(err, "Failed to add `PrometheusRule` API to scheme")

		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&quayredhatcomv1.QuayRegistry{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		// TODO: Add `.Owns()` for every resource type we manage...
		Complete(r)
}

// patchNamespaceForMonitoring Adds the cluster-monitoring label to the namespace if needed
func (r *QuayRegistryReconciler) patchNamespaceForMonitoring(ctx context.Context, quay v1.QuayRegistry) error {
	var ns corev1.Namespace
	err := r.Client.Get(ctx, types.NamespacedName{Name: quay.GetNamespace()}, &ns)

	if err != nil {
		return err
	}

	updatedNs := ns.DeepCopy()
	labels := make(map[string]string)
	for k, v := range updatedNs.Labels {
		labels[k] = v
	}

	// We only add the `cluster-monitoring` label when it's not already present. In this case
	// We also add another label to make sure we clean up correctly
	// i.e remove the label only when we added it
	if val, ok := labels[ClusterMonitoringLabelKey]; !ok || val != "true" {
		labels[ClusterMonitoringLabelKey] = "true"
		labels[QuayOperatorManagedLabelKey] = "true"
		updatedNs.Labels = labels

		patch := client.MergeFrom(&ns)
		err = r.Client.Patch(context.Background(), updatedNs, patch)
		return err
	}

	return nil
}

// cleanupNamespaceLabels Cleans up the monitoring label if we added it on the namespace
// This runs as part of the finalizer which is invoked when a registry is deleted
func (r *QuayRegistryReconciler) cleanupNamespaceLabels(ctx context.Context, quay *v1.QuayRegistry) error {
	var ns corev1.Namespace
	err := r.Client.Get(ctx, types.NamespacedName{Name: quay.GetNamespace()}, &ns)

	if err != nil {
		return err
	}

	var quayRegistryList v1.QuayRegistryList
	listOps := client.ListOptions{
		Namespace: quay.GetNamespace(),
	}

	if err := r.Client.List(ctx, &quayRegistryList, &listOps); err != nil {
		return err
	}

	// Only update if we had initially added the label and this is the last QuayRegistry in the namespace
	if ns.Labels != nil && ns.Labels[QuayOperatorManagedLabelKey] != "" && len(quayRegistryList.Items) == 1 {
		updatedNs := ns.DeepCopy()
		labels := make(map[string]string)
		for k, v := range updatedNs.Labels {
			labels[k] = v
		}
		delete(labels, ClusterMonitoringLabelKey)
		delete(labels, QuayOperatorManagedLabelKey)
		updatedNs.Labels = labels

		patch := client.MergeFrom(&ns)
		err = r.Client.Patch(context.Background(), updatedNs, patch)
		return err
	}

	return nil
}

// finalizeQuay runs the Cleanup operations when a `QuayRegistry` is deleted
func (r *QuayRegistryReconciler) finalizeQuay(ctx context.Context, quay *v1.QuayRegistry) error {
	return r.cleanupNamespaceLabels(ctx, quay)
}
