package cassandrarestore

import (
	"context"
	errors "emperror.dev/errors"
	"fmt"
	"github.com/Orange-OpenSource/casskop/pkg/apis/db/v1alpha1"
	"github.com/Orange-OpenSource/casskop/pkg/backrest"
	"github.com/Orange-OpenSource/casskop/pkg/controller/common"
	"github.com/Orange-OpenSource/casskop/pkg/errorfactory"
	"github.com/Orange-OpenSource/casskop/pkg/k8s"
	"github.com/Orange-OpenSource/casskop/pkg/util"
	"github.com/sirupsen/logrus"
	"k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"time"
)

// ReconcileCassandraCluster reconciles a CassandraCluster object
type ReconcileCassandraRestore struct {
	// This client, initialized using mgr.Client() above, is a split client
	// that reads objects from the cache and writes to the apiserver
	cc       *v1alpha1.CassandraCluster
	recorder record.EventRecorder
	client   client.Client
	scheme   *runtime.Scheme
}

func (r ReconcileCassandraRestore) Reconcile(request reconcile.Request) (reconcile.Result, error) {

	reqLogger := logrus.WithFields(logrus.Fields{"Request.Namespace": request.Namespace, "Request.Name": request.Name})
	reqLogger.Info("Reconciling CassandraBackup")

	ctx := context.TODO()

	// Fetch the CassandraRestore cassandraRestore
	cassandraRestore := &v1alpha1.CassandraRestore{}
	err := r.client.Get(ctx, request.NamespacedName, cassandraRestore)

	if err != nil {
		if k8sErrors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return common.Reconciled()
		}
		// Error reading the object - requeue the request.
		return common.RequeueWithError(reqLogger, err.Error(), err)
	}

	// Check the referenced Cluster exists.
	cassandraCluster := &v1alpha1.CassandraCluster{}
	if cassandraCluster, err = k8s.LookupCassandraCluster(r.client, cassandraRestore.Spec.CassandraCluster,
		cassandraRestore.Namespace); err != nil {
		// This shouldn't trigger anymore, but leaving it here as a safetybelt
		if k8s.IsMarkedForDeletion(cassandraRestore.ObjectMeta) {
			reqLogger.Info("Cluster is gone already, there is nothing we can do")
			return common.Reconciled()
		}
		r.recorder.Event(
			cassandraRestore,
			v1.EventTypeWarning,
			"CassandraClusterNotFound",
			fmt.Sprintf("Cassandra Cluster %s to restore not found", cassandraRestore.Spec.CassandraCluster))
		return common.RequeueWithError(reqLogger, "failed to lookup referenced cluster", err)
	}

	// Check the referenced Backup exists.
	cassandraBackup := &v1alpha1.CassandraBackup{}
	if cassandraBackup, err = k8s.LookupCassandraBackup(r.client, cassandraRestore.Spec.CassandraBackup,
		cassandraRestore.Namespace); err != nil {
		r.recorder.Event(
			cassandraRestore,
			v1.EventTypeWarning,
			"BackupNotFound",
			fmt.Sprintf("Backup %s to restore not found", cassandraRestore.Spec.CassandraBackup))
		return common.RequeueWithError(reqLogger, "failed to lookup referenced cassandraBackup", err)
	}

	// Require restore on first pod of the cluster.
	if cassandraRestore.Status.Condition == nil {
		err = r.requiredRestore(cassandraRestore, cassandraCluster, cassandraBackup, reqLogger)
		if err != nil {
			switch errors.Cause(err).(type) {
			case errorfactory.ResourceNotReady:
				return controllerruntime.Result{
					RequeueAfter: time.Duration(15) * time.Second,
				}, nil
			default:
				return common.RequeueWithError(reqLogger, err.Error(), err)
			}
		}
		r.recorder.Event(cassandraRestore,
			v1.EventTypeNormal,
			"RestoreRequired",
			fmt.Sprintf("Restore task required from cassandraBackup of datacenter %s of cluster %s to %s under snapshot %s. Restore operation on pod %s",
				cassandraBackup.Spec.Datacenter, cassandraBackup.Spec.CassandraCluster,
				cassandraBackup.Spec.StorageLocation, cassandraBackup.Spec.SnapshotTag,
				cassandraRestore.Status.CoordinatorMember))
	}

	if len(cassandraRestore.Status.CoordinatorMember) == 0 {
		return common.RequeueWithError(reqLogger, "No coordinator member to perform the restore", err)
	}

	if cassandraRestore.Status.Condition.Type.IsRequired() {
		err = r.handleRequiredRestore(cassandraRestore, cassandraCluster, cassandraBackup, reqLogger)
		if err != nil {
			switch errors.Cause(err).(type) {
			case errorfactory.CassandraBackupSidecarNotReady, errorfactory.ResourceNotReady:
				r.recorder.Event(
					cassandraRestore,
					v1.EventTypeWarning,
					"PerformRestoreOperationFailed",
					fmt.Sprintf("Restore task from cassandraBackup of datacenter %s of cluster %s to %s under snapshot %s failed to run, will retry. Restore operation on pod %s", cassandraBackup.Spec.Datacenter, cassandraBackup.Spec.CassandraCluster,
						cassandraBackup.Spec.StorageLocation, cassandraBackup.Spec.SnapshotTag,
						cassandraRestore.Status.CoordinatorMember))
				return controllerruntime.Result{
					RequeueAfter: time.Duration(15) * time.Second,
				}, nil
			default:
				return common.RequeueWithError(reqLogger, err.Error(), err)
			}
		}
		r.recorder.Event(cassandraRestore,
			v1.EventTypeNormal,
			"RestoreInitiated",
			fmt.Sprintf("Restore task initiated from cassandraBackup of datacenter %s of cluster %s to %s under snapshot %s. Restore operation %v on pod %s.",
				cassandraBackup.Spec.Datacenter, cassandraBackup.Spec.CassandraCluster,
				cassandraBackup.Spec.StorageLocation, cassandraBackup.Spec.SnapshotTag,
				cassandraRestore.Status.ID, cassandraRestore.Status.CoordinatorMember))
	}

	if cassandraRestore.Status.Condition.Type.IsInProgress() {

		err = r.checkRestoreOperationState(cassandraRestore, cassandraCluster, cassandraBackup, reqLogger)
		if err != nil {
			switch errors.Cause(err).(type) {
			case errorfactory.CassandraBackupSidecarNotReady, errorfactory.ResourceNotReady:
				return controllerruntime.Result{
					RequeueAfter: time.Duration(15) * time.Second,
				}, nil
			case errorfactory.CassandraBackupOperationRunning:
				return controllerruntime.Result{
					RequeueAfter: time.Duration(20) * time.Second,
				}, nil
			case errorfactory.CassandraBackupOperationFailure:
				return controllerruntime.Result{
					RequeueAfter: time.Duration(20) * time.Second,
				}, nil
			default:
				return common.RequeueWithError(reqLogger, err.Error(), err)
			}
		}
		r.recorder.Event(cassandraRestore,
			v1.EventTypeNormal,
			"RestoreCompleted",
			fmt.Sprintf("Restore task from cassandraBackup of datacenter %s of cluster %s to %s under snapshot %s is completed. Restore operation %v on pod %s.",
				cassandraBackup.Spec.Datacenter, cassandraBackup.Spec.CassandraCluster,
				cassandraBackup.Spec.StorageLocation, cassandraBackup.Spec.SnapshotTag,
				cassandraRestore.Status.ID, cassandraRestore.Status.CoordinatorMember))
	}
	return common.Reconciled()
}

// requiredRestore select restore coordinator on a specific member of a Cluster
func (r *ReconcileCassandraRestore) requiredRestore(restore *v1alpha1.CassandraRestore, cc *v1alpha1.CassandraCluster,
	backup *v1alpha1.CassandraBackup, reqLogger *logrus.Entry) error {
	ns := restore.Namespace

	pods, err := r.listPods(ns, k8s.LabelsForCassandraDC(cc, backup.Spec.Datacenter))
	if err != nil {
		return errorfactory.New(errorfactory.ResourceNotReady{}, err, "No pods founds for this dc")
	}

	if len(pods.Items) > 0 {
		if err := UpdateRestoreStatus(r.client, restore,
			v1alpha1.CassandraRestoreStatus{
				Condition: &v1alpha1.RestoreCondition{
					Type:               v1alpha1.RestoreRequired,
					LastTransitionTime: v12.Now().Format(util.TimeStampLayout),
				},
				CoordinatorMember: pods.Items[0].Name,
			}, reqLogger); err != nil {
			return errors.WrapIfWithDetails(err, "Could not update status for restore",
				"restore", restore)
		}

		return nil
	}

	return errors.New("No pods found.")
}

func (r *ReconcileCassandraRestore) handleRequiredRestore(restore *v1alpha1.CassandraRestore,
	cc *v1alpha1.CassandraCluster, backup *v1alpha1.CassandraBackup, reqLogger *logrus.Entry) error {
	pods, err := r.listPods(restore.Namespace, k8s.LabelsForCassandraDC(cc, backup.Spec.Datacenter))
	if err != nil {
		return errorfactory.New(errorfactory.ResourceNotReady{}, err, "no pods founds for this dc")
	}

	sr, err := backrest.NewClient(r.client, cc, &pods.Items[random.Intn(len(pods.Items))])
	if err != nil {
		reqLogger.Info("Cassandra backup sidecar communication error checking running restore operation")
		return errorfactory.New(errorfactory.CassandraBackupSidecarNotReady{}, err, "sidecar communication error")
	}

	restoreStatus, err := sr.PerformRestore(restore, backup)
	if err != nil {
		reqLogger.Info("Cassandra sidecar communication error checking running restore operation")
		return errorfactory.New(errorfactory.CassandraBackupSidecarNotReady{}, err,
		"cassandra backup sidecar communication error")
	}

	restoreStatus.CoordinatorMember = restore.Status.CoordinatorMember
	if err := UpdateRestoreStatus(r.client, restore, *restoreStatus, reqLogger); err != nil {
		return errors.WrapIfWithDetails(err, "could not update status for restore", "restore", restore)
	}

	return nil
}

func (r *ReconcileCassandraRestore) checkRestoreOperationState(restore *v1alpha1.CassandraRestore,
	cc *v1alpha1.CassandraCluster, backup *v1alpha1.CassandraBackup, reqLogger *logrus.Entry) error {

	pods, err := r.listPods(restore.Namespace, k8s.LabelsForCassandraDC(cc, backup.Spec.Datacenter))
	if err != nil {
		return errorfactory.New(errorfactory.ResourceNotReady{}, err, "no pods founds for this dc")
	}

	restoreId := restore.Status.ID
	if restoreId == "" {
		return errors.New("no Restore operation id provided to be checked")
	}

	// Check Restore operation status
	sr, err := backrest.NewClient(r.client, cc, k8s.PodByName(pods, restore.Status.CoordinatorMember))
	if err != nil {
		reqLogger.Info("cassandra backup sidecar communication error checking running Operation", "OperationId",
			restoreId)
		return errorfactory.New(errorfactory.CassandraBackupSidecarNotReady{}, err,
		"cassandra backup sidecar communication error")
	}
	restoreStatus, err := sr.RestoreStatusByID(restoreId)
	if err != nil {
		reqLogger.Info("cassandra backup sidecar communication error checking running Operation",
			"OperationId", restoreId)
		return errorfactory.New(errorfactory.CassandraBackupSidecarNotReady{}, err,
		"cassandra backup sidecar communication error")
	}

	restoreStatus.CoordinatorMember = restore.Status.CoordinatorMember
	if err := UpdateRestoreStatus(r.client, restore, *restoreStatus, reqLogger); err != nil {
		return errors.WrapIfWithDetails(err, "could not update status for restore",
			"restore", restore)
	}

	// Restore operation failed or canceled,
	// TODO : reschedule it by marking restore Condition.State = RestoreRequired ?
	if restore.Status.Condition.Type.IsInError() {
		return errorfactory.New(errorfactory.CassandraBackupOperationFailure{}, err,
		"cassandra backup sidecar Operation failed", fmt.Sprintf("restore operation id : %s", restoreId))
	}

	// Restore operation completed successfully
	if restore.Status.Condition.Type.IsCompleted() {
		return nil
	}

	// TODO : Implement timeout ?

	// restore operation still in progress
	reqLogger.Info("Cassandra backup sidecar operation is still running", "restoreId", restoreId)
	return errorfactory.New(errorfactory.CassandraBackupOperationRunning{},
		errors.New("cassandra backup sidecar restore operation still running"),
			fmt.Sprintf("restore operation id : %s", restoreId))
}

func (r *ReconcileCassandraRestore) updateAndFetchLatest(ctx context.Context,
	restore *v1alpha1.CassandraRestore) (*v1alpha1.CassandraRestore, error) {
	typeMeta := restore.TypeMeta
	err := r.client.Update(ctx, restore)
	if err != nil {
		return nil, err
	}
	restore.TypeMeta = typeMeta
	return restore, nil
}

func (r *ReconcileCassandraRestore) listPods(namespace string, selector map[string]string) (*v1.PodList, error) {

	clientOpt := &client.ListOptions{
		Namespace:     namespace,
		LabelSelector: labels.SelectorFromSet(selector),
	}

	opt := []client.ListOption{
		clientOpt,
	}

	pl := &v1.PodList{}
	return pl, r.client.List(context.TODO(), pl, opt...)
}