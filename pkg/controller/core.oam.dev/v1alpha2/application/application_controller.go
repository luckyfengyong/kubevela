/*
Copyright 2021 The KubeVela Authors.

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

package application

import (
	"context"
	"time"

	"github.com/crossplane/crossplane-runtime/apis/core/v1alpha1"
	"github.com/crossplane/crossplane-runtime/pkg/event"
	"github.com/crossplane/crossplane-runtime/pkg/logging"
	"github.com/crossplane/crossplane-runtime/pkg/meta"
	"github.com/go-logr/logr"
	"github.com/pkg/errors"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	velatypes "github.com/oam-dev/kubevela/apis/types"
	"github.com/oam-dev/kubevela/pkg/appfile"
	core "github.com/oam-dev/kubevela/pkg/controller/core.oam.dev"
	"github.com/oam-dev/kubevela/pkg/dsl/definition"
	"github.com/oam-dev/kubevela/pkg/oam/discoverymapper"
	oamutil "github.com/oam-dev/kubevela/pkg/oam/util"
	"github.com/oam-dev/kubevela/pkg/utils/apply"
)

// RolloutReconcileWaitTime is the time to wait before reconcile again an application still in rollout phase
const (
	RolloutReconcileWaitTime      = time.Second * 3
	resourceTrackerFinalizer      = "resourceTracker.finalizer.core.oam.dev"
	errUpdateApplicationStatus    = "cannot update application status"
	errUpdateApplicationFinalizer = "cannot update application finalizer"
)

// Reconciler reconciles a Application object
type Reconciler struct {
	client.Client
	dm               discoverymapper.DiscoveryMapper
	pd               *definition.PackageDiscover
	Log              logr.Logger
	Scheme           *runtime.Scheme
	Recorder         event.Recorder
	applicator       apply.Applicator
	appRevisionLimit int
}

// +kubebuilder:rbac:groups=core.oam.dev,resources=applications,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.oam.dev,resources=applications/status,verbs=get;update;patch

// Reconcile process app event
func (r *Reconciler) Reconcile(req ctrl.Request) (ctrl.Result, error) {
	ctx := context.Background()
	applog := r.Log.WithValues("application", req.NamespacedName)
	app := new(v1beta1.Application)
	if err := r.Get(ctx, client.ObjectKey{
		Name:      req.Name,
		Namespace: req.Namespace,
	}, app); err != nil {
		if kerrors.IsNotFound(err) {
			err = nil
		}
		return ctrl.Result{}, err
	}

	handler := &appHandler{
		r:      r,
		app:    app,
		logger: applog,
	}

	if app.ObjectMeta.DeletionTimestamp.IsZero() {
		if registerFinalizers(app) {
			applog.Info("Register new finalizer", "application", app.Namespace+"/"+app.Name, "finalizers", app.ObjectMeta.Finalizers)
			return reconcile.Result{}, errors.Wrap(r.Client.Update(ctx, app), errUpdateApplicationFinalizer)
		}
	} else {
		needUpdate, err := handler.removeResourceTracker(ctx)
		if err != nil {
			applog.Error(err, "Failed to remove application resourceTracker")
			app.Status.SetConditions(v1alpha1.ReconcileError(errors.Wrap(err, "error to  remove finalizer")))
			return reconcile.Result{}, errors.Wrap(r.UpdateStatus(ctx, app), errUpdateApplicationStatus)
		}
		if needUpdate {
			applog.Info("remove finalizer of application", "application", app.Namespace+"/"+app.Name, "finalizers", app.ObjectMeta.Finalizers)
			return ctrl.Result{}, errors.Wrap(r.Update(ctx, app), errUpdateApplicationFinalizer)
		}
		// deleting and no need to handle finalizer
		return reconcile.Result{}, nil
	}

	applog.Info("Start Rendering")

	app.Status.Phase = common.ApplicationRendering

	applog.Info("parse template")
	// parse template
	appParser := appfile.NewApplicationParser(r.Client, r.dm, r.pd)

	ctx = oamutil.SetNamespaceInCtx(ctx, app.Namespace)
	generatedAppfile, err := appParser.GenerateAppFile(ctx, app)
	if err != nil {
		applog.Error(err, "[Handle Parse]")
		app.Status.SetConditions(errorCondition("Parsed", err))
		r.Recorder.Event(app, event.Warning(velatypes.ReasonFailedParse, err))
		return handler.handleErr(err)
	}

	app.Status.SetConditions(readyCondition("Parsed"))
	handler.appfile = generatedAppfile

	appRev, err := handler.GenerateAppRevision(ctx)
	if err != nil {
		applog.Error(err, "[Handle Calculate Revision]")
		app.Status.SetConditions(errorCondition("Parsed", err))
		r.Recorder.Event(app, event.Warning(velatypes.ReasonFailedParse, err))
		return handler.handleErr(err)
	}
	r.Recorder.Event(app, event.Normal(velatypes.ReasonParsed, velatypes.MessageParsed))
	// Record the revision so it can be used to render data in context.appRevision
	generatedAppfile.RevisionName = appRev.Name

	applog.Info("build template")
	// build template to applicationconfig & component
	ac, comps, err := generatedAppfile.GenerateApplicationConfiguration()
	if err != nil {
		applog.Error(err, "[Handle GenerateApplicationConfiguration]")
		app.Status.SetConditions(errorCondition("Built", err))
		r.Recorder.Event(app, event.Warning(velatypes.ReasonFailedRender, err))
		return handler.handleErr(err)
	}

	err = handler.handleResourceTracker(ctx, comps, ac)
	if err != nil {
		applog.Error(err, "[Handle resourceTracker]")
		app.Status.SetConditions(errorCondition("Handle resourceTracker", err))
		r.Recorder.Event(app, event.Warning(velatypes.ReasonFailedRender, err))
		return handler.handleErr(err)
	}

	// pass the App label and annotation to ac except some app specific ones
	oamutil.PassLabelAndAnnotation(app, ac)

	app.Status.SetConditions(readyCondition("Built"))
	r.Recorder.Event(app, event.Normal(velatypes.ReasonRendered, velatypes.MessageRendered))
	applog.Info("apply application revision & component to the cluster")
	// apply application revision & component to the cluster
	if err := handler.apply(ctx, appRev, ac, comps); err != nil {
		applog.Error(err, "[Handle apply]")
		app.Status.SetConditions(errorCondition("Applied", err))
		r.Recorder.Event(app, event.Warning(velatypes.ReasonFailedApply, err))
		return handler.handleErr(err)
	}

	// if inplace is false and rolloutPlan is nil, it means the user will use an outer AppRollout object to rollout the application
	if handler.app.Spec.RolloutPlan != nil {
		res, err := handler.handleRollout(ctx)
		if err != nil {
			applog.Error(err, "[handle rollout]")
			app.Status.SetConditions(errorCondition("Rollout", err))
			r.Recorder.Event(app, event.Warning(velatypes.ReasonFailedRollout, err))
			return handler.handleErr(err)
		}
		// skip health check and garbage collection if rollout have not finished
		// start next reconcile immediately
		if res.Requeue || res.RequeueAfter > 0 {
			app.Status.Phase = common.ApplicationRollingOut
			return res, r.UpdateStatus(ctx, app)
		}

		// there is no need reconcile immediately, that means the rollout operation have finished
		r.Recorder.Event(app, event.Normal(velatypes.ReasonRollout, velatypes.MessageRollout))
		app.Status.SetConditions(readyCondition("Rollout"))
		applog.Info("rollout finished")
	}

	// The following logic will be skipped if rollout have not finished
	app.Status.SetConditions(readyCondition("Applied"))
	r.Recorder.Event(app, event.Normal(velatypes.ReasonFailedApply, velatypes.MessageApplied))
	app.Status.Phase = common.ApplicationHealthChecking
	applog.Info("check application health status")
	// check application health status
	appCompStatus, healthy, err := handler.statusAggregate(generatedAppfile)
	if err != nil {
		applog.Error(err, "[status aggregate]")
		app.Status.SetConditions(errorCondition("HealthCheck", err))
		r.Recorder.Event(app, event.Warning(velatypes.ReasonFailedHealthCheck, err))
		return handler.handleErr(err)
	}
	if !healthy {
		app.Status.SetConditions(errorCondition("HealthCheck", errors.New("not healthy")))

		app.Status.Services = appCompStatus
		// unhealthy will check again after 10s
		return ctrl.Result{RequeueAfter: time.Second * 10}, r.Status().Update(ctx, app)
	}
	app.Status.Services = appCompStatus
	app.Status.SetConditions(readyCondition("HealthCheck"))
	r.Recorder.Event(app, event.Normal(velatypes.ReasonHealthCheck, velatypes.MessageHealthCheck))
	app.Status.Phase = common.ApplicationRunning

	err = garbageCollection(ctx, handler)
	if err != nil {
		applog.Error(err, "[Garbage collection]")
		r.Recorder.Event(app, event.Warning(velatypes.ReasonFailedGC, err))
	}

	// Gather status of components
	var refComps []v1alpha1.TypedReference
	for _, comp := range comps {
		refComps = append(refComps, v1alpha1.TypedReference{
			APIVersion: comp.APIVersion,
			Kind:       comp.Kind,
			Name:       comp.Name,
			UID:        app.UID,
		})
	}
	app.Status.Components = refComps
	r.Recorder.Event(app, event.Normal(velatypes.ReasonDeployed, velatypes.MessageDeployed))
	return ctrl.Result{}, r.UpdateStatus(ctx, app)
}

// if any finalizers newly registered, return true
func registerFinalizers(app *v1beta1.Application) bool {
	if !meta.FinalizerExists(&app.ObjectMeta, resourceTrackerFinalizer) && app.Status.ResourceTracker != nil {
		meta.AddFinalizer(&app.ObjectMeta, resourceTrackerFinalizer)
		return true
	}
	return false
}

// SetupWithManager install to manager
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	// If Application Own these two child objects, AC status change will notify application controller and recursively update AC again, and trigger application event again...
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1beta1.Application{}).
		Complete(r)
}

// UpdateStatus updates v1beta1.Application's Status with retry.RetryOnConflict
func (r *Reconciler) UpdateStatus(ctx context.Context, app *v1beta1.Application, opts ...client.UpdateOption) error {
	status := app.DeepCopy().Status
	return retry.RetryOnConflict(retry.DefaultBackoff, func() (err error) {
		if err = r.Get(ctx, types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, app); err != nil {
			return
		}
		app.Status = status
		return r.Status().Update(ctx, app, opts...)
	})
}

// Setup adds a controller that reconciles AppRollout.
func Setup(mgr ctrl.Manager, args core.Args, _ logging.Logger) error {
	reconciler := Reconciler{
		Client:           mgr.GetClient(),
		Log:              ctrl.Log.WithName("Application"),
		Scheme:           mgr.GetScheme(),
		Recorder:         event.NewAPIRecorder(mgr.GetEventRecorderFor("Application")),
		dm:               args.DiscoveryMapper,
		pd:               args.PackageDiscover,
		applicator:       apply.NewAPIApplicator(mgr.GetClient()),
		appRevisionLimit: args.AppRevisionLimit,
	}
	return reconciler.SetupWithManager(mgr)
}
