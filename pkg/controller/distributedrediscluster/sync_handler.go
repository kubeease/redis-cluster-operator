package distributedrediscluster

import (
	"fmt"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"

	redisv1alpha1 "github.com/ucloud/redis-cluster-operator/pkg/apis/redis/v1alpha1"
	"github.com/ucloud/redis-cluster-operator/pkg/controller/clustering"
	"github.com/ucloud/redis-cluster-operator/pkg/controller/manager"
	"github.com/ucloud/redis-cluster-operator/pkg/k8sutil"
	"github.com/ucloud/redis-cluster-operator/pkg/redisutil"
	"github.com/ucloud/redis-cluster-operator/pkg/resources/statefulsets"
)

const (
	requeueAfter = 10 * time.Second
)

type syncContext struct {
	cluster      *redisv1alpha1.DistributedRedisCluster
	clusterInfos *redisutil.ClusterInfos
	admin        redisutil.IAdmin
	healer       manager.IHeal
	pods         []*corev1.Pod
	reqLogger    logr.Logger
}

func (r *ReconcileDistributedRedisCluster) ensureCluster(ctx *syncContext) error {
	cluster := ctx.cluster
	if err := r.validate(cluster, ctx.reqLogger); err != nil {
		if k8sutil.IsRequestRetryable(err) {
			return Kubernetes.Wrap(err, "Validate")
		}
		return StopRetry.Wrap(err, "stop retry")
	}
	labels := getLabels(cluster)
	var backup *redisv1alpha1.RedisClusterBackup
	var err error
	if cluster.Spec.Init != nil {
		backup, err = r.crController.GetRedisClusterBackup(cluster.Spec.Init.BackupSource.Namespace, cluster.Spec.Init.BackupSource.Name)
		if err != nil {
			return err
		}
	}
	if err := r.ensurer.EnsureRedisConfigMap(cluster, labels); err != nil {
		return Kubernetes.Wrap(err, "EnsureRedisConfigMap")
	}
	if updated, err := r.ensurer.EnsureRedisStatefulsets(cluster, backup, labels); err != nil {
		ctx.reqLogger.Error(err, "EnsureRedisStatefulSets")
		return Kubernetes.Wrap(err, "EnsureRedisStatefulSets")
	} else if updated {
		waiter := &waitStatefulSetUpdating{
			name:                  "waitStatefulSetUpdating",
			timeout:               30 * time.Second * time.Duration(cluster.Spec.ClusterReplicas+2),
			tick:                  5 * time.Second,
			statefulSetController: r.statefulSetController,
			cluster:               cluster,
		}
		if err := waiting(waiter, ctx.reqLogger); err != nil {
			return err
		}
	}
	if err := r.ensurer.EnsureRedisHeadLessSvcs(cluster, labels); err != nil {
		return Kubernetes.Wrap(err, "EnsureRedisHeadLessSvcs")
	}
	if err := r.ensurer.EnsureRedisSvc(cluster, labels); err != nil {
		return Kubernetes.Wrap(err, "EnsureRedisSvc")
	}
	if err := r.ensurer.EnsureRedisOSMSecret(cluster, backup, labels); err != nil {
		if k8sutil.IsRequestRetryable(err) {
			return Kubernetes.Wrap(err, "EnsureRedisOSMSecret")
		}
		return StopRetry.Wrap(err, "stop retry")
	}
	return nil
}

func (r *ReconcileDistributedRedisCluster) waitPodReady(ctx *syncContext) error {
	if _, err := ctx.healer.FixTerminatingPods(ctx.cluster, 5*time.Minute); err != nil {
		return Kubernetes.Wrap(err, "FixTerminatingPods")
	}
	if err := r.checker.CheckRedisNodeNum(ctx.cluster); err != nil {
		return Requeue.Wrap(err, "CheckRedisNodeNum")
	}

	return nil
}

func (r *ReconcileDistributedRedisCluster) validate(cluster *redisv1alpha1.DistributedRedisCluster, reqLogger logr.Logger) error {
	initSpec := cluster.Spec.Init
	if initSpec != nil {
		if initSpec.BackupSource == nil {
			return fmt.Errorf("backupSource is required")
		}
		backup, err := r.crController.GetRedisClusterBackup(initSpec.BackupSource.Namespace, initSpec.BackupSource.Name)
		if err != nil {
			return err
		}
		if backup.Status.Phase != redisv1alpha1.BackupPhaseSucceeded {
			return fmt.Errorf("backup is still running")
		}
		if cluster.Spec.Image == "" {
			cluster.Spec.Image = backup.Status.ClusterImage
		}
		cluster.Spec.MasterSize = backup.Status.MasterSize
		if cluster.Status.RestoreSucceeded <= 0 {
			cluster.Spec.ClusterReplicas = 0
		} else {
			cluster.Spec.ClusterReplicas = backup.Status.ClusterReplicas
		}
	}
	cluster.Validate(reqLogger)
	return nil
}

func (r *ReconcileDistributedRedisCluster) waitForClusterJoin(ctx *syncContext) error {
	if infos, err := ctx.admin.GetClusterInfos(); err == nil {
		ctx.reqLogger.V(6).Info("debug waitForClusterJoin", "cluster infos", infos)
		return nil
	}
	var firstNode *redisutil.Node
	for _, nodeInfo := range ctx.clusterInfos.Infos {
		firstNode = nodeInfo.Node
		break
	}
	ctx.reqLogger.Info(">>> Sending CLUSTER MEET messages to join the cluster")
	err := ctx.admin.AttachNodeToCluster(firstNode.IPPort())
	if err != nil {
		return Redis.Wrap(err, "AttachNodeToCluster")
	}
	// Give one second for the join to start, in order to avoid that
	// waiting for cluster join will find all the nodes agree about
	// the config as they are still empty with unassigned slots.
	time.Sleep(1 * time.Second)

	_, err = ctx.admin.GetClusterInfos()
	if err != nil {
		return Requeue.Wrap(err, "wait for cluster join")
	}
	return nil
}

func (r *ReconcileDistributedRedisCluster) syncCluster(ctx *syncContext) error {
	cluster := ctx.cluster
	admin := ctx.admin
	clusterInfos := ctx.clusterInfos
	expectMasterNum := cluster.Spec.MasterSize
	rCluster, nodes, err := newRedisCluster(clusterInfos, cluster)
	if err != nil {
		return Cluster.Wrap(err, "newRedisCluster")
	}
	clusterCtx := clustering.NewCtx(rCluster, nodes, cluster.Spec.MasterSize, cluster.Name, ctx.reqLogger)
	if err := clusterCtx.DispatchMasters(); err != nil {
		return Cluster.Wrap(err, "DispatchMasters")
	}
	curMasters := clusterCtx.GetCurrentMasters()
	newMasters := clusterCtx.GetNewMasters()
	ctx.reqLogger.Info("masters", "newMasters", len(newMasters), "curMasters", len(curMasters))
	if len(curMasters) == 0 {
		ctx.reqLogger.Info("Creating cluster")
		if err := clusterCtx.PlaceSlaves(); err != nil {
			return Cluster.Wrap(err, "PlaceSlaves")

		}
		if err := clusterCtx.AttachingSlavesToMaster(admin); err != nil {
			return Cluster.Wrap(err, "AttachingSlavesToMaster")
		}

		if err := clusterCtx.AllocSlots(admin, newMasters); err != nil {
			return Cluster.Wrap(err, "AllocSlots")
		}
	} else if len(newMasters) > len(curMasters) {
		ctx.reqLogger.Info("Scaling up")
		if err := clusterCtx.PlaceSlaves(); err != nil {
			return Cluster.Wrap(err, "PlaceSlaves")

		}
		if err := clusterCtx.AttachingSlavesToMaster(admin); err != nil {
			return Cluster.Wrap(err, "AttachingSlavesToMaster")
		}

		if err := clusterCtx.RebalancedCluster(admin, newMasters); err != nil {
			return Cluster.Wrap(err, "RebalancedCluster")
		}
	} else if cluster.Status.MinReplicationFactor < cluster.Spec.ClusterReplicas {
		ctx.reqLogger.Info("Scaling slave")
		if err := clusterCtx.PlaceSlaves(); err != nil {
			return Cluster.Wrap(err, "PlaceSlaves")

		}
		if err := clusterCtx.AttachingSlavesToMaster(admin); err != nil {
			return Cluster.Wrap(err, "AttachingSlavesToMaster")
		}
	} else if len(curMasters) > int(expectMasterNum) {
		ctx.reqLogger.Info("Scaling down")
		var allMaster redisutil.Nodes
		allMaster = append(allMaster, newMasters...)
		allMaster = append(allMaster, curMasters...)
		if err := clusterCtx.DispatchSlotToNewMasters(admin, newMasters, curMasters, allMaster); err != nil {
			return err
		}
		if err := r.scalingDown(ctx, len(curMasters), clusterCtx.GetStatefulsetNodes()); err != nil {
			return err
		}
	}
	return nil
}

func (r *ReconcileDistributedRedisCluster) scalingDown(ctx *syncContext, currentMasterNum int, statefulSetNodes map[string]redisutil.Nodes) error {
	cluster := ctx.cluster
	admin := ctx.admin
	expectMasterNum := int(cluster.Spec.MasterSize)
	for i := currentMasterNum - 1; i >= expectMasterNum; i-- {
		stsName := statefulsets.ClusterStatefulSetName(cluster.Name, i)
		for _, node := range statefulSetNodes[stsName] {
			admin.Connections().Remove(node.IPPort())
		}
	}
	for i := currentMasterNum - 1; i >= expectMasterNum; i-- {
		stsName := statefulsets.ClusterStatefulSetName(cluster.Name, i)
		ctx.reqLogger.Info("scaling down", "statefulSet", stsName)
		sts, err := r.statefulSetController.GetStatefulSet(cluster.Namespace, stsName)
		if err != nil {
			return Kubernetes.Wrap(err, "GetStatefulSet")
		}
		for _, node := range statefulSetNodes[stsName] {
			ctx.reqLogger.Info("forgetNode", "id", node.ID, "ip", node.IP, "role", node.GetRole())
			if len(node.Slots) > 0 {
				return Redis.New(fmt.Sprintf("node %s is not empty! Reshard data away and try again", node.String()))
			}
			if err := admin.ForgetNode(node.ID); err != nil {
				return Redis.Wrap(err, "ForgetNode")
			}
		}
		// remove resource
		if err := r.statefulSetController.DeleteStatefulSetByName(cluster.Namespace, stsName); err != nil {
			ctx.reqLogger.Error(err, "DeleteStatefulSetByName", "statefulSet", stsName)
		}
		svcName := statefulsets.ClusterHeadlessSvcName(cluster.Name, i)
		if err := r.serviceController.DeleteServiceByName(cluster.Namespace, svcName); err != nil {
			ctx.reqLogger.Error(err, "DeleteServiceByName", "service", svcName)
		}
		if err := r.pdbController.DeletePodDisruptionBudgetByName(cluster.Namespace, stsName); err != nil {
			ctx.reqLogger.Error(err, "DeletePodDisruptionBudgetByName", "pdb", stsName)
		}
		if err := r.pvcController.DeletePvcByLabels(cluster.Namespace, sts.Labels); err != nil {
			ctx.reqLogger.Error(err, "DeletePvcByLabels", "labels", sts.Labels)
		}
		// wait pod Terminating
		waiter := &waitPodTerminating{
			name:                  "waitPodTerminating",
			statefulSet:           stsName,
			timeout:               30 * time.Second * time.Duration(cluster.Spec.ClusterReplicas+2),
			tick:                  5 * time.Second,
			statefulSetController: r.statefulSetController,
			cluster:               cluster,
		}
		if err := waiting(waiter, ctx.reqLogger); err != nil {
			ctx.reqLogger.Error(err, "waitPodTerminating")
		}

	}
	return nil
}
