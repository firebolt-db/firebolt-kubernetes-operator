/*
Copyright 2025.

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

// ensureAccountInitialized checks that the metadata service has exactly one
// active account. If zero accounts exist it creates and activates one. The
// resolved account ID is persisted in the instance status.
func (r *FireboltInstanceReconciler) ensureAccountInitialized(ctx context.Context, instance *computev1alpha1.FireboltInstance) (string, error) {
	log := logf.FromContext(ctx)

	if instance.Status.AccountID != "" {
		return instance.Status.AccountID, nil
	}

	conn, cleanup, err := r.dialMetadataService(ctx, instance)
	if err != nil {
		return "", fmt.Errorf("dial metadata service: %w", err)
	}
	defer cleanup()

	txClient := transactionv1.NewTransactionServiceClient(conn)
	adminClient := adminv2.NewAdminServiceClient(conn)

	roResp, err := txClient.StartReadOnlyAdminTransaction(ctx, &transactionv1.StartReadOnlyAdminTransactionRequest{})
	if err != nil {
		return "", fmt.Errorf("StartReadOnlyAdminTransaction: %w", err)
	}
	roTxID := roResp.GetAdminTransactionId()

	accounts, states, err := listAllAccounts(ctx, adminClient, roTxID)
	if err != nil {
		return "", fmt.Errorf("GetAccounts: %w", err)
	}

	switch len(accounts) {
	case 1:
		if states[0] == adminv2.AccountState_ACCOUNT_STATE_ACTIVE {
			log.Info("Metadata service account already active", "accountId", accounts[0])
			return accounts[0], nil
		}
		log.Info("Metadata service account exists but is not active, attempting activation",
			"accountId", accounts[0], "state", states[0])
		if err := activateAccount(ctx, txClient, adminClient, accounts[0]); err != nil {
			return "", fmt.Errorf("activating existing account %s: %w", accounts[0], err)
		}
		log.Info("Metadata service account activated", "accountId", accounts[0])
		return accounts[0], nil

	case 0:
		log.Info("No accounts found in metadata service, creating initial account")
		accountID, err := createAndProvisionAccount(ctx, txClient, adminClient)
		if err != nil {
			return "", fmt.Errorf("failed to create initial account: %w", err)
		}
		log.Info("Initial metadata service account created and activated", "accountId", accountID)
		return accountID, nil

	default:
		instance.Status.Phase = computev1alpha1.InstancePhaseFailed
		if updateErr := r.Status().Update(ctx, instance); updateErr != nil {
			log.Error(updateErr, "Failed to update instance status to Failed")
		}
		return "", fmt.Errorf("metadata service has %d accounts; expected exactly 1; manual intervention required", len(accounts))
	}
}

// dialMetadataService establishes a gRPC connection to the metadata service
// via its in-cluster DNS name.
func (r *FireboltInstanceReconciler) dialMetadataService(ctx context.Context, instance *computev1alpha1.FireboltInstance) (*grpc.ClientConn, func(), error) {
	endpoint := metadataServiceEndpoint(instance.Name, instance.Namespace)

	conn, err := grpc.NewClient(endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dialing metadata service at %s: %w", endpoint, err)
	}
	return conn, func() { conn.Close() }, nil
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
		if err == io.EOF {
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
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	tx1, err := txClient.StartAdminTransaction(ctx, &transactionv1.StartAdminTransactionRequest{})
	if err != nil {
		return "", fmt.Errorf("StartAdminTransaction (create): %w", err)
	}

	createResp, err := adminClient.CreateAccount(ctx, &adminv2.CreateAccountRequest{
		AdminTransactionId: tx1.GetAdminTransactionId(),
		Blob:               []byte("{}"),
	})
	if err != nil {
		return "", fmt.Errorf("CreateAccount: %w", err)
	}
	accountID := createResp.GetAccountId()

	_, err = txClient.CommitAdminTransaction(ctx, &transactionv1.CommitAdminTransactionRequest{
		AdminTransactionId: tx1.GetAdminTransactionId(),
	})
	if err != nil {
		return "", fmt.Errorf("CommitAdminTransaction (create): %w", err)
	}

	if err := activateAccount(ctx, txClient, adminClient, accountID); err != nil {
		return "", err
	}

	return accountID, nil
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
