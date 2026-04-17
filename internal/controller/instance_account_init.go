/*
Copyright 2026 Firebolt Analytics.

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

package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	adminv2 "github.com/firebolt-analytics/pensieve/gen/proto/go/admin/v2"
	transactionv1 "github.com/firebolt-analytics/pensieve/gen/proto/go/transaction/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	computev1alpha1 "github.com/firebolt-analytics/firebolt-kubernetes-operator/api/v1alpha1"
)

// ensureAccountInitialized ensures that the metadata service has an active
// account whose ID matches instance.Spec.ID. If no accounts exist it creates
// one using CreateAccountWithID. If the account already exists (e.g. from a
// previous reconcile) it is activated if necessary.
func (r *FireboltInstanceReconciler) ensureAccountInitialized(ctx context.Context, instance *computev1alpha1.FireboltInstance) error {
	log := logf.FromContext(ctx)
	instanceID := instance.Spec.ID

	if instanceID == "" {
		return fmt.Errorf("FireboltInstance %q has no spec.id; defaulting webhook may not be running", instance.Name)
	}

	// Bound the whole account-init flow with a single deadline. Without this,
	// a dial that hangs (metadata pod NotReady, network partition, DNS stall)
	// or a server-side RPC that never returns would park the reconciler on
	// this object indefinitely, starving other FireboltInstances that share
	// the controller worker pool. 30s is comfortably longer than a healthy
	// end-to-end create+activate (6 serial RPCs over localhost-scale gRPC)
	// while short enough to retry on the next requeue if something is stuck.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// AccountReady is a one-way latch. Once the operator has observed the
	// metadata account in ACTIVE state (or just created + activated it), we
	// never re-check. This trades reactivity for cost: each reconcile would
	// otherwise dial metadata, open a read-only admin tx, and stream all
	// accounts — work that buys nothing once the account is known active.
	//
	// The trade-off is that we will NOT auto-recover if an out-of-band
	// actor (e.g. a human admin, a migration tool) deactivates the account
	// after the latch has flipped. Recovery in that case requires flipping
	// Status.AccountReady back to false (kubectl patch, or delete+recreate
	// the FireboltInstance) to force a re-evaluation of the switch below.
	if instance.Status.AccountReady {
		return nil
	}

	conn, cleanup, err := r.dialMetadataService(ctx, instance)
	if err != nil {
		return fmt.Errorf("dial metadata service: %w", err)
	}
	defer cleanup()

	txClient := transactionv1.NewTransactionServiceClient(conn)
	adminClient := adminv2.NewAdminServiceClient(conn)

	roResp, err := txClient.StartReadOnlyAdminTransaction(ctx, &transactionv1.StartReadOnlyAdminTransactionRequest{})
	if err != nil {
		return fmt.Errorf("StartReadOnlyAdminTransaction: %w", err)
	}
	roTxID := roResp.GetAdminTransactionId()

	accounts, states, err := listAllAccounts(ctx, adminClient, roTxID)
	if err != nil {
		return fmt.Errorf("GetAccounts: %w", err)
	}

	switch len(accounts) {
	case 1:
		if accounts[0] != instanceID {
			instance.Status.Phase = computev1alpha1.InstancePhaseFailed
			if updateErr := r.Status().Update(ctx, instance); updateErr != nil {
				log.Error(updateErr, "Failed to update instance status to Failed")
			}
			return fmt.Errorf("metadata account %q does not match instance ID %q; manual intervention required", accounts[0], instanceID)
		}
		if states[0] == adminv2.AccountState_ACCOUNT_STATE_ACTIVE {
			log.Info("Metadata service account already active", "accountId", accounts[0])
			instance.Status.AccountReady = true
			return nil
		}
		log.Info("Metadata service account exists but is not active, attempting activation",
			"accountId", accounts[0], "state", states[0])
		if err := activateAccount(ctx, txClient, adminClient, accounts[0]); err != nil {
			return fmt.Errorf("activating existing account %s: %w", accounts[0], err)
		}
		log.Info("Metadata service account activated", "accountId", accounts[0])
		instance.Status.AccountReady = true
		return nil

	case 0:
		log.Info("No accounts found in metadata service, creating initial account", "accountId", instanceID)
		if err := createAndProvisionAccount(ctx, txClient, adminClient, instanceID); err != nil {
			return fmt.Errorf("failed to create initial account: %w", err)
		}
		log.Info("Initial metadata service account created and activated", "accountId", instanceID)
		instance.Status.AccountReady = true
		return nil

	default:
		instance.Status.Phase = computev1alpha1.InstancePhaseFailed
		if updateErr := r.Status().Update(ctx, instance); updateErr != nil {
			log.Error(updateErr, "Failed to update instance status to Failed")
		}
		return fmt.Errorf("metadata service has %d accounts; expected exactly 1; manual intervention required", len(accounts))
	}
}

// dialMetadataService establishes a gRPC connection to the metadata service.
// If r.DialMetadata is set (e.g. in E2E tests), it delegates to that function;
// otherwise it dials via in-cluster DNS.
func (r *FireboltInstanceReconciler) dialMetadataService(ctx context.Context, instance *computev1alpha1.FireboltInstance) (*grpc.ClientConn, func(), error) {
	if r.DialMetadata != nil {
		return r.DialMetadata(ctx, instance)
	}

	endpoint := metadataServiceEndpoint(instance.Name, instance.Namespace)

	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dialing metadata service at %s: %w", endpoint, err)
	}
	return conn, func() { _ = conn.Close() }, nil
}

func listAllAccounts(ctx context.Context, client adminv2.AdminServiceClient, adminTxID string) (ids []string, states []adminv2.AccountState, err error) {
	stream, err := client.GetAccounts(ctx, &adminv2.GetAccountsRequest{
		AdminTransactionId: adminTxID,
	})
	if err != nil {
		return nil, nil, err
	}
	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		acct := resp.GetAccount()
		if acct == nil {
			continue
		}
		ids = append(ids, acct.GetId())
		states = append(states, resp.GetAccountState())
	}
	return ids, states, nil
}

func createAndProvisionAccount(
	ctx context.Context,
	txClient transactionv1.TransactionServiceClient,
	adminClient adminv2.AdminServiceClient,
	accountID string,
) error {
	// The caller (ensureAccountInitialized) wraps ctx with a 30s deadline
	// that covers this whole flow plus the dial and the preceding listAll;
	// no additional timeout is needed here.

	tx1, err := txClient.StartAdminTransaction(ctx, &transactionv1.StartAdminTransactionRequest{})
	if err != nil {
		return fmt.Errorf("StartAdminTransaction (create): %w", err)
	}

	_, err = adminClient.CreateAccountWithID(ctx, &adminv2.CreateAccountWithIDRequest{
		AdminTransactionId: tx1.GetAdminTransactionId(),
		Id:                 accountID,
		Blob:               []byte("{}"),
	})
	if err != nil {
		return fmt.Errorf("CreateAccountWithID: %w", err)
	}

	_, err = txClient.CommitAdminTransaction(ctx, &transactionv1.CommitAdminTransactionRequest{
		AdminTransactionId: tx1.GetAdminTransactionId(),
	})
	if err != nil {
		return fmt.Errorf("CommitAdminTransaction (create): %w", err)
	}

	return activateAccount(ctx, txClient, adminClient, accountID)
}

// activateAccount activates an existing account in a dedicated transaction.
// This is called both during initial provisioning and when recovering an
// account left in a non-active state by a previously interrupted reconcile.
func activateAccount(
	ctx context.Context,
	txClient transactionv1.TransactionServiceClient,
	adminClient adminv2.AdminServiceClient,
	accountID string,
) error {
	tx, err := txClient.StartAdminTransaction(ctx, &transactionv1.StartAdminTransactionRequest{})
	if err != nil {
		return fmt.Errorf("StartAdminTransaction (activate): %w", err)
	}

	_, err = adminClient.ActivateAccount(ctx, &adminv2.ActivateAccountRequest{
		AdminTransactionId: tx.GetAdminTransactionId(),
		AccountId:          accountID,
	})
	if err != nil {
		return fmt.Errorf("ActivateAccount (account=%s): %w", accountID, err)
	}

	_, err = txClient.CommitAdminTransaction(ctx, &transactionv1.CommitAdminTransactionRequest{
		AdminTransactionId: tx.GetAdminTransactionId(),
	})
	if err != nil {
		return fmt.Errorf("CommitAdminTransaction (activate): %w", err)
	}

	return nil
}
