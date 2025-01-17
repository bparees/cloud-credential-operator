package azure

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/msi/armmsi"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	azureclients "github.com/openshift/cloud-credential-operator/pkg/azure"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
)

var (
	// DeleteOpts captures the azureOptions that affect deletion of the identity provider
	// and managed identities
	DeleteOpts = azureOptions{}
)

// deleteManagedIdentities lists user-assigned managed identities and deletes those with CCO's "owned" tag.
func deleteManagedIdentities(client *azureclients.AzureClientWrapper, name, resourceGroupName, subscriptionID, region string) error {
	listManagedIdentities := client.UserAssignedIdentitiesClient.NewListByResourceGroupPager(
		resourceGroupName,
		&armmsi.UserAssignedIdentitiesClientListByResourceGroupOptions{},
	)
	ownedTagKey := fmt.Sprintf("%s_%s", ownedAzureResourceTagKeyPrefix, name)
	managedIdentities := make([]*armmsi.Identity, 0)
	for listManagedIdentities.More() {
		pageResponse, err := listManagedIdentities.NextPage(context.Background())
		if err != nil {
			return err
		}
		// Find managed identities within the resource group that have CCO's "owned" tag.
		// The "owned" tag key includes the name argument provided to "ccoctl create-managed-identities"
		// so ccoctl will only delete identites that ccoctl created.
		//
		// Key: "openshift.io_cloud-credential-operator_<name>"
		// Value: "owned"
		for _, identity := range pageResponse.UserAssignedIdentitiesListResult.Value {
			if nameTagValue, found := identity.Tags[ownedTagKey]; found && *nameTagValue == ownedAzureResourceTagValue {
				managedIdentities = append(managedIdentities, identity)
			}
		}
	}
	if len(managedIdentities) == 0 {
		log.Printf("Found no user-assigned managed identities with tag key=%s, value=%s", ownedTagKey, ownedAzureResourceTagValue)
		return nil
	}
	for _, identity := range managedIdentities {
		_, err := client.UserAssignedIdentitiesClient.Delete(
			context.Background(),
			resourceGroupName,
			*identity.Name,
			&armmsi.UserAssignedIdentitiesClientDeleteOptions{},
		)
		if err != nil {
			return err
		}
		log.Printf("Deleted %s %s", *identity.Type, *identity.ID)
	}
	return nil
}

func deleteResourceGroup(client *azureclients.AzureClientWrapper, resourceGroupName string) error {
	pollerResp, err := client.ResourceGroupsClient.BeginDelete(
		context.Background(),
		resourceGroupName,
		&armresources.ResourceGroupsClientBeginDeleteOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to delete resource group")
	}
	// Stomped return is an armresources.ResourceGroupsClientDeleteResponse which is an empty struct with no values
	_, err = pollerResp.PollUntilDone(context.Background(), &runtime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	if err != nil {
		return err
	}
	log.Printf("Deleted resource group %s", resourceGroupName)
	return nil
}

func deleteStorageAccount(client *azureclients.AzureClientWrapper, resourceGroupName, storageAccountName string) error {
	_, err := client.StorageAccountClient.Delete(
		context.Background(),
		resourceGroupName,
		storageAccountName,
		&armstorage.AccountsClientDeleteOptions{})
	if err != nil {
		return errors.Wrap(err, "failed to delete storage account")
	}
	log.Printf("Deleted storage account %s", storageAccountName)
	return nil
}

func deleteCmd(cmd *cobra.Command, args []string) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		log.Fatal(err)
	}

	azureClientWrapper, err := azureclients.NewAzureClientWrapper(DeleteOpts.SubscriptionID, cred, &policy.ClientOptions{}, false)
	if err != nil {
		log.Fatal("Failed to create Azure client")
	}

	if DeleteOpts.OIDCResourceGroupName == "" {
		DeleteOpts.OIDCResourceGroupName = DeleteOpts.Name + oidcResourceGroupSuffix
		log.Printf("No --oidc-resource-group-name provided, defaulting OIDC resource group name to %s", DeleteOpts.OIDCResourceGroupName)
	}

	if DeleteOpts.StorageAccountName == "" {
		DeleteOpts.StorageAccountName = DeleteOpts.Name
		log.Printf("No --storage-account-name provided, defaulting storage account name to %s", DeleteOpts.StorageAccountName)
	}
	if err := validateStorageAccountName(DeleteOpts.StorageAccountName); err != nil {
		log.Fatal(err)
	}

	// Every Azure object created by ccoctl exists within the context of the OIDC resource group so deleting the OIDC resource group
	// will delete everything within and we can return after the resource group has been deleted
	if DeleteOpts.DeleteOIDCResourceGroup {
		err = deleteResourceGroup(
			azureClientWrapper,
			DeleteOpts.OIDCResourceGroupName)
		if err != nil {
			log.Fatal(err)
		}
		return
	}

	// Delete user-assigned managed identities
	err = deleteManagedIdentities(azureClientWrapper,
		DeleteOpts.Name,
		DeleteOpts.OIDCResourceGroupName,
		DeleteOpts.SubscriptionID,
		DeleteOpts.Region)
	if err != nil {
		log.Fatal(err)
	}

	// Delete storage account
	err = deleteStorageAccount(azureClientWrapper,
		DeleteOpts.OIDCResourceGroupName,
		DeleteOpts.StorageAccountName)
	if err != nil {
		log.Fatal(err)
	}
}

// NewDeleteCmd provides the "delete" subcommand
func NewDeleteCmd() *cobra.Command {
	deleteCmd := &cobra.Command{
		Use:   "delete --name NAME --region REGION --subscription-id SUBSCRIPTION_ID",
		Short: "Delete OIDC issuer and managed identities",
		Long: "This command will delete the storage account and user-assigned managed identities within the OIDC resource group by default. " +
			"The OIDC resource group will be deleted when the --delete-oidc-resource-group paramter has been provided.",
		Run: deleteCmd,
	}

	// Required
	deleteCmd.PersistentFlags().StringVar(&DeleteOpts.Name, "name", "", "User-defined name for all previously created Azure resources")
	deleteCmd.MarkPersistentFlagRequired("name")
	deleteCmd.PersistentFlags().StringVar(&DeleteOpts.Region, "region", "", "Azure region in which to delete user-assigned managed identities")
	deleteCmd.MarkPersistentFlagRequired("region")
	deleteCmd.PersistentFlags().StringVar(&DeleteOpts.SubscriptionID, "subscription-id", "", "Azure Subscription ID within which to create and scope the access of managed identities")
	deleteCmd.MarkPersistentFlagRequired("subscription-id")

	// Optional
	deleteCmd.PersistentFlags().BoolVar(
		&DeleteOpts.DeleteOIDCResourceGroup,
		"delete-oidc-resource-group",
		false,
		"Delete the OIDC resource group that is identified by --oidc-resource-group-name parameter if specified. "+
			"If --oidc-resource-group-name is not specified, the name of the OIDC resource group will be derived from the --name parameter.",
	)
	deleteCmd.PersistentFlags().StringVar(
		&DeleteOpts.StorageAccountName,
		"storage-account-name",
		"",
		"The name of the Azure storage account to delete. "+
			"The storage account must exist within the OIDC resource group identified by the --oidc-resource-group-name parameter "+
			"or within the OIDC resource group name derived from the --name parameter when --oidc-resource-group-name paramter was not provided. "+
			"Azure storage account names must be between 3 and 24 characters in length and may contain numbers and lowercase letters only.",
	)
	// TODO: Plumb dry-run through delete
	deleteCmd.PersistentFlags().BoolVar(&DeleteOpts.DryRun, "dry-run", false, "Skip deleting objects and display actions that would have been taken")
	deleteCmd.PersistentFlags().StringVar(&DeleteOpts.OIDCResourceGroupName, "oidc-resource-group-name", "", "The Azure resource group in which to delete user-assigned managed identities. This resource group will not be deleted unless --delete-resource-group has been specified.")

	return deleteCmd
}
