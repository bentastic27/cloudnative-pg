/*
This file is part of Cloud Native PostgreSQL.

Copyright (C) 2019-2021 EnterpriseDB Corporation.
*/

package controller

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"time"

	"github.com/lib/pq"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/util/retry"

	apiv1 "github.com/EnterpriseDB/cloud-native-postgresql/api/v1"
	"github.com/EnterpriseDB/cloud-native-postgresql/internal/management/utils"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/postgres/metrics"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/management/postgres/webserver"
	"github.com/EnterpriseDB/cloud-native-postgresql/pkg/postgres"
)

// Reconcile is the main reconciliation loop for the instance
func (r *InstanceReconciler) Reconcile(ctx context.Context, event *watch.Event) error {
	r.log.Info(
		"Reconciliation loop",
		"eventType", event.Type,
		"type", event.Object.GetObjectKind().GroupVersionKind())

	kind := event.Object.GetObjectKind().GroupVersionKind().Kind
	switch kind {
	case "Cluster":
		return r.reconcileCluster(ctx, event)
	case "ConfigMap":
		return r.reconcileConfigMap(ctx, event)
	case "Secret":
		return r.reconcileSecret(event)
	default:
		r.log.Info("unknown reconciliation target, skipped event",
			"kind", kind)
	}

	return nil
}

// reconcileCluster is called when something is changed at the
// cluster level
func (r *InstanceReconciler) reconcileCluster(ctx context.Context, event *watch.Event) error {
	object, err := objectToUnstructured(event.Object)
	if err != nil {
		return fmt.Errorf(
			"decoding runtime.Object data from watch: %w",
			err)
	}

	var cluster apiv1.Cluster
	err = runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &cluster)
	if err != nil {
		return fmt.Errorf("error decoding cluster resource: %w", err)
	}

	// Reconcile monitoring section
	if cluster.Spec.Monitoring != nil {
		r.reconcileMonitoringQueries(ctx, cluster.Spec.Monitoring)
	}

	// Reconcile replica role
	if cluster.Status.TargetPrimary == r.instance.PodName {
		// This is a primary server
		err := r.reconcilePrimary(ctx, object)
		if err != nil {
			if event.Type == watch.Added {
				// TODO: find a way to reschedule the Added event
				r.log.Info(
					"WARNING: Cannot configure instance permissions due to a failure reconciling as primary",
					"error", err)
			}
			return err
		}

		// Apply all the settings required by the operator if this is the first time we
		// this instance.
		if event.Type == watch.Added {
			return r.configureInstancePermissions()
		}

		return nil
	}

	return r.reconcileReplica()
}

// reconcileMonitoringQueries applies the custom monitoring queries to the
// web server
func (r *InstanceReconciler) reconcileMonitoringQueries(
	ctx context.Context,
	configuration *apiv1.MonitoringConfiguration,
) {
	r.log.Info("Reconciling custom monitoring queries")

	queries := metrics.NewQueriesCollector("cnp", r.instance)

	for _, reference := range configuration.CustomQueriesConfigMap {
		configMap, err := r.GetStaticClient().CoreV1().ConfigMaps(r.instance.Namespace).Get(
			ctx, reference.Name, metav1.GetOptions{})
		if err != nil {
			r.log.Info("Unable to get configMap containing custom monitoring queries",
				"reference", reference,
				"clusterName", r.instance.ClusterName,
				"namespace", r.instance.Namespace)
			continue
		}

		data, ok := configMap.Data[reference.Key]
		if !ok {
			r.log.Info("Missing key in configMap",
				"reference", reference,
				"clusterName", r.instance.ClusterName,
				"namespace", r.instance.Namespace)
			continue
		}

		err = queries.ParseQueries([]byte(data))
		if err != nil {
			r.log.Info("Error while parsing custom queries in ConfigMap",
				"reference", reference,
				"clusterName", r.instance.ClusterName,
				"namespace", r.instance.Namespace,
				"error", err.Error())
			continue
		}
	}

	for _, reference := range configuration.CustomQueriesSecret {
		secret, err := r.GetStaticClient().CoreV1().Secrets(r.instance.Namespace).Get(
			ctx, reference.Name, metav1.GetOptions{})
		if err != nil {
			r.log.Info("Unable to get secret containing custom monitoring queries",
				"reference", reference,
				"clusterName", r.instance.ClusterName,
				"namespace", r.instance.Namespace)
			continue
		}

		data, ok := secret.Data[reference.Key]
		if !ok {
			r.log.Info("Missing key in secret",
				"reference", reference,
				"clusterName", r.instance.ClusterName,
				"namespace", r.instance.Namespace)
			continue
		}

		err = queries.ParseQueries(data)
		if err != nil {
			r.log.Info("Error while parsing custom queries in Secret",
				"reference", reference,
				"clusterName", r.instance.ClusterName,
				"namespace", r.instance.Namespace,
				"error", err.Error())
			continue
		}
	}

	exporter := webserver.GetExporter()
	exporter.SetCustomQueries(queries)
}

// reconcileSecret is called when the PostgreSQL secrets are changes
func (r *InstanceReconciler) reconcileSecret(event *watch.Event) error {
	if event.Type == watch.Added {
		// The bootstrap status has been already
		// been applied when the instance started up.
		// No need to reconcile here.
		return nil
	}

	object, err := objectToUnstructured(event.Object)
	if err != nil {
		return fmt.Errorf(
			"decoding runtime.Object data from watch: %w",
			err)
	}

	name, err := utils.GetName(object)
	if err != nil {
		return fmt.Errorf("while reading secret name: %w", err)
	}

	switch {
	case strings.HasSuffix(name, apiv1.ServerSecretSuffix):
		err = r.refreshCertificateFilesFromObject(
			object,
			postgres.ServerCertificateLocation,
			postgres.ServerKeyLocation)
		if err != nil {
			return err
		}

	case strings.HasSuffix(name, apiv1.ReplicationSecretSuffix):
		err = r.refreshCertificateFilesFromObject(
			object,
			postgres.StreamingReplicaCertificateLocation,
			postgres.StreamingReplicaKeyLocation)
		if err != nil {
			return err
		}

	case strings.HasSuffix(name, apiv1.CaSecretSuffix):
		err = r.refreshCAFromObject(object)
		if err != nil {
			return err
		}
	}

	r.log.Info("reloading the TLS crypto material")
	err = r.instance.Reload()
	if err != nil {
		return fmt.Errorf("while applying new certificates: %w", err)
	}

	return nil
}

// reconcileConfigMap is called then the ConfigMap generated by the
// cluster changes
func (r *InstanceReconciler) reconcileConfigMap(ctx context.Context, event *watch.Event) error {
	object, err := objectToUnstructured(event.Object)
	if err != nil {
		return fmt.Errorf(
			"decoding runtime.Object data from watch: %w",
			err)
	}

	changed, err := r.instance.RefreshConfigurationFilesFromObject(object)
	if err != nil {
		return err
	}

	if !changed {
		r.log.Info("PostgreSQL configuration has not been changed")
		return nil
	}

	// This function could also be called while the server is being
	// started up, so we are not sure that the server is really active.
	// Let's wait for that.
	err = r.instance.WaitForSuperuserConnectionAvailable()
	if err != nil {
		return fmt.Errorf("while applying new configuration: %w", err)
	}

	// Ok, now we're ready to SIGHUP this server
	err = r.instance.Reload()
	if err != nil {
		return fmt.Errorf("while applying new configuration: %w", err)
	}

	// TODO: we already sighup the postgres server and
	// probably it has already reloaded the configuration
	// anyway there's no guarantee here that the signal
	// has been actually received and sent to the children.
	// What shall we do? Wait for a bit of time? Or inject
	// a configuration marker and wait for it to appear somewhere?
	status, err := r.instance.GetStatus()
	if err != nil {
		return fmt.Errorf("while applying new configuration: %w", err)
	}

	if !status.PendingRestart {
		// Everything fine
		return nil
	}

	cluster, err := r.client.
		Resource(apiv1.ClusterGVK).
		Namespace(r.instance.Namespace).
		Get(ctx, r.instance.ClusterName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("while applying new configuration: %w", err)
	}

	// TODO: stop here if the phase is already "Applying configuration"
	err = utils.SetPhase(cluster, "Applying configuration", "PostgreSQL configuration changed")
	if err != nil {
		return err
	}

	// Let's wake up the operator as I need to be restarted
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		_, err = r.client.
			Resource(apiv1.ClusterGVK).
			Namespace(r.instance.Namespace).
			UpdateStatus(ctx, cluster, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}

		// If we have a conflict, let's replace the cluster info
		// with one more fresh
		if apierrors.IsConflict(err) {
			r.log.Info(
				"Conflict detected while setting current primary, retrying",
				"err", err.Error())

			cluster, err = r.client.
				Resource(apiv1.ClusterGVK).
				Namespace(r.instance.Namespace).
				Get(ctx, r.instance.ClusterName, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("while applying new configuration: %w", err)
			}

			// TODO: stop here if the phase is already "Applying configuration"
			err = utils.SetPhase(cluster, "Applying configuration", "PostgreSQL configuration changed")
			if err != nil {
				return err
			}
		}
		return err
	})
}

// refreshConfigurationFilesFromObject receive an unstructured object representing
// a secret and rewrite the file corresponding to the server certificate
func (r *InstanceReconciler) refreshCertificateFilesFromObject(
	object *unstructured.Unstructured,
	certificateLocation string,
	privateKeyLocation string,
) error {
	certificate, err := utils.GetCertificate(object)
	if err != nil {
		return err
	}

	privateKey, err := utils.GetPrivateKey(object)
	if err != nil {
		return err
	}

	certificateBytes, err := base64.StdEncoding.DecodeString(certificate)
	if err != nil {
		return fmt.Errorf("while reading server certificate: %w", err)
	}

	privateKeyBytes, err := base64.StdEncoding.DecodeString(privateKey)
	if err != nil {
		return fmt.Errorf("while reading server private key: %w", err)
	}

	err = ioutil.WriteFile(certificateLocation, certificateBytes, 0600)
	if err != nil {
		return fmt.Errorf("while writing server certificate: %w", err)
	}

	err = ioutil.WriteFile(privateKeyLocation, privateKeyBytes, 0600)
	if err != nil {
		return fmt.Errorf("while writing server private key: %w", err)
	}

	return nil
}

// refreshConfigurationFilesFromObject receive an unstructured object representing
// a secret and rewrite the file corresponding to the server certificate
func (r *InstanceReconciler) refreshCAFromObject(object *unstructured.Unstructured) error {
	caCertificate, err := utils.GetCACertificate(object)
	if err != nil {
		return err
	}

	caCertificateBytes, err := base64.StdEncoding.DecodeString(caCertificate)
	if err != nil {
		return fmt.Errorf("while reading CA certificate: %w", err)
	}

	err = ioutil.WriteFile(postgres.CACertificateLocation, caCertificateBytes, 0600)
	if err != nil {
		return fmt.Errorf("while writing server certificate: %w", err)
	}

	return nil
}

// Reconciler primary logic
func (r *InstanceReconciler) reconcilePrimary(ctx context.Context, cluster *unstructured.Unstructured) error {
	isPrimary, err := r.instance.IsPrimary()
	if err != nil {
		return err
	}

	if isPrimary {
		// All right
		return nil
	}

	r.log.Info("I'm the target primary, wait for the wal_receiver to be terminated")

	err = r.waitForWalReceiverDown()
	if err != nil {
		return err
	}

	r.log.Info("I'm the target primary, wait for every pending WAL record to be applied")

	err = r.waitForApply()
	if err != nil {
		return err
	}

	r.log.Info("I'm the target primary, promoting my instance")

	// I must promote my instance here
	err = r.instance.PromoteAndWait()
	if err != nil {
		return fmt.Errorf("error promoting instance: %w", err)
	}

	// Now I'm the primary, need to inform the operator
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		r.log.Info("Setting myself as the current primary")
		err = utils.SetCurrentPrimary(cluster, r.instance.PodName)
		if err != nil {
			return err
		}

		_, err = r.client.
			Resource(apiv1.ClusterGVK).
			Namespace(r.instance.Namespace).
			UpdateStatus(ctx, cluster, metav1.UpdateOptions{})
		if err == nil {
			return nil
		}

		// If we have a conflict, let's replace the cluster info
		// with one more fresh
		if apierrors.IsConflict(err) {
			r.log.Info(
				"Conflict detected while setting current primary, retrying",
				"err", err.Error())

			var errRefresh error
			cluster, errRefresh = r.client.
				Resource(apiv1.ClusterGVK).
				Namespace(r.instance.Namespace).
				Get(ctx, r.instance.ClusterName, metav1.GetOptions{})

			if errRefresh != nil {
				r.log.Error(errRefresh, "Error while refreshing cluster info")
			}
		}
		return err
	})
}

// Reconciler replica logic
func (r *InstanceReconciler) reconcileReplica() error {
	isPrimary, err := r.instance.IsPrimary()
	if err != nil {
		return err
	}

	if !isPrimary {
		// All right
		return nil
	}

	r.log.Info("This is an old primary node. Requesting a checkpoint before demotion")

	db, err := r.instance.GetSuperUserDB()
	if err != nil {
		r.log.Error(err, "Cannot connect to primary server")
	} else {
		_, err = db.Exec("CHECKPOINT")
		if err != nil {
			r.log.Error(err, "Error while requesting a checkpoint")
		}
	}

	r.log.Info("This is an old primary node. Shutting it down to get it demoted to a replica")

	// I was the primary, but now I'm not the primary anymore.
	// Here we need to invoke a fast shutdown on the instance, and wait the the pod
	// restart to demote as a replica of the new primary
	return r.instance.Shutdown()
}

// objectToUnstructured convert a runtime Object into an unstructured one
func objectToUnstructured(object runtime.Object) (*unstructured.Unstructured, error) {
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object)
	if err != nil {
		return nil, err
	}

	return &unstructured.Unstructured{Object: data}, nil
}

// waitForApply wait for every transaction log to be applied
func (r *InstanceReconciler) waitForApply() error {
	// TODO: exponential backoff
	for {
		lag, err := r.instance.GetWALApplyLag()
		if err != nil {
			return err
		}

		if lag <= 0 {
			break
		}

		r.log.Info("Still need to apply transaction log info, waiting for 1 second",
			"lag", lag)
		time.Sleep(time.Second * 1)
	}

	return nil
}

// waitForWalReceiverDown wait until the wal receiver is down, and it's used
// to grab all the WAL files from a replica
func (r *InstanceReconciler) waitForWalReceiverDown() error {
	// TODO: exponential backoff
	for {
		status, err := r.instance.IsWALReceiverActive()
		if err != nil {
			return err
		}

		if !status {
			break
		}

		r.log.Info("WAL receiver is still active, waiting for 2 seconds")
		time.Sleep(time.Second * 1)
	}

	return nil
}

// configureInstancePermissions creates the expected users and databases in a new
// PostgreSQL instance
func (r *InstanceReconciler) configureInstancePermissions() error {
	var err error

	majorVersion, err := postgres.GetMajorVersion(r.instance.PgData)
	if err != nil {
		return fmt.Errorf("while getting major version: %w", err)
	}

	db, err := r.instance.GetSuperUserDB()
	if err != nil {
		return fmt.Errorf("while getting a connection to the instance: %w", err)
	}

	r.log.Info("Waiting for server to start")
	err = r.instance.WaitForSuperuserConnectionAvailable()
	if err != nil {
		r.log.Error(err, "server did not start in time")
		os.Exit(1)
	}

	r.log.Info("Configuring primary instance")

	// A transaction is required to temporarily disable synchronous replication
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("creating a new transaction to setup the instance: %w", err)
	}

	_, err = tx.Exec("SET LOCAL synchronous_commit TO LOCAL")
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	hasSuperuser, err := r.configureStreamingReplicaUser(tx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	err = r.configurePgRewindPrivileges(majorVersion, hasSuperuser, tx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	return tx.Commit()
}

// configureStreamingReplicaUser makes sure the the streaming replication user exists
// and has the required rights
func (r *InstanceReconciler) configureStreamingReplicaUser(tx *sql.Tx) (bool, error) {
	var hasLoginRight, hasReplicationRight, hasSuperuser bool
	row := tx.QueryRow("SELECT rolcanlogin, rolreplication, rolsuper FROM pg_roles WHERE rolname = $1",
		apiv1.StreamingReplicationUser)
	err := row.Scan(&hasLoginRight, &hasReplicationRight, &hasSuperuser)
	if err != nil {
		if err == sql.ErrNoRows {
			_, err = tx.Exec(fmt.Sprintf(
				"CREATE USER %v REPLICATION",
				pq.QuoteIdentifier(apiv1.StreamingReplicationUser)))
			if err != nil {
				return false, fmt.Errorf("CREATE USER %v error: %w", apiv1.StreamingReplicationUser, err)
			}
		} else {
			return false, fmt.Errorf("while creating streaming replication user: %w", err)
		}
	}

	if !hasLoginRight || !hasReplicationRight {
		_, err = tx.Exec(fmt.Sprintf(
			"ALTER USER %v LOGIN REPLICATION",
			pq.QuoteIdentifier(apiv1.StreamingReplicationUser)))
		if err != nil {
			return false, fmt.Errorf("ALTER USER %v error: %w", apiv1.StreamingReplicationUser, err)
		}
	}
	return hasSuperuser, nil
}

// configurePgRewindPrivileges ensures that the StreamingReplicationUser has enough rights to execute pg_rewind
func (r *InstanceReconciler) configurePgRewindPrivileges(majorVersion int, hasSuperuser bool, tx *sql.Tx) error {
	// We need the superuser bit for the streaming-replication user since pg_rewind in PostgreSQL <= 10
	// will require it.
	if majorVersion <= 10 {
		if !hasSuperuser {
			_, err := tx.Exec(fmt.Sprintf(
				"ALTER USER %v SUPERUSER",
				pq.QuoteIdentifier(apiv1.StreamingReplicationUser)))
			if err != nil {
				return fmt.Errorf("ALTER USER %v error: %w", apiv1.StreamingReplicationUser, err)
			}
		}
		return nil
	}

	// Ensure the user has rights to execute the functions needed for pg_rewind
	var hasPgRewindPrivileges bool
	row := tx.QueryRow(
		`
			SELECT has_function_privilege($1, 'pg_ls_dir(text, boolean, boolean)', 'execute') AND
			       has_function_privilege($2, 'pg_stat_file(text, boolean)', 'execute') AND
			       has_function_privilege($3, 'pg_read_binary_file(text)', 'execute') AND
			       has_function_privilege($4, 'pg_read_binary_file(text, bigint, bigint, boolean)', 'execute')`,
		apiv1.StreamingReplicationUser,
		apiv1.StreamingReplicationUser,
		apiv1.StreamingReplicationUser,
		apiv1.StreamingReplicationUser)
	err := row.Scan(&hasPgRewindPrivileges)
	if err != nil {
		return fmt.Errorf("while getting streaming replication user privileges: %w", err)
	}

	if !hasPgRewindPrivileges {
		_, err = tx.Exec(fmt.Sprintf(
			"GRANT EXECUTE ON function pg_catalog.pg_ls_dir(text, boolean, boolean) TO %v",
			pq.QuoteIdentifier(apiv1.StreamingReplicationUser)))
		if err != nil {
			return fmt.Errorf("while granting pgrewind privileges: %w", err)
		}

		_, err = tx.Exec(fmt.Sprintf(
			"GRANT EXECUTE ON function pg_catalog.pg_stat_file(text, boolean) TO %v",
			pq.QuoteIdentifier(apiv1.StreamingReplicationUser)))
		if err != nil {
			return fmt.Errorf("while granting pgrewind privileges: %w", err)
		}

		_, err = tx.Exec(fmt.Sprintf(
			"GRANT EXECUTE ON function pg_catalog.pg_read_binary_file(text) TO %v",
			pq.QuoteIdentifier(apiv1.StreamingReplicationUser)))
		if err != nil {
			return fmt.Errorf("while granting pgrewind privileges: %w", err)
		}

		_, err = tx.Exec(fmt.Sprintf(
			"GRANT EXECUTE ON function pg_catalog.pg_read_binary_file(text, bigint, bigint, boolean) TO %v",
			pq.QuoteIdentifier(apiv1.StreamingReplicationUser)))
		if err != nil {
			return fmt.Errorf("while granting pgrewind privileges: %w", err)
		}
	}

	return nil
}
