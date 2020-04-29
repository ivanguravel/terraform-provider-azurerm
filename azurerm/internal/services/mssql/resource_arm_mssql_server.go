package mssql

import (
	"fmt"
	"log"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/preview/sql/mgmt/v3.0/sql"
	"github.com/hashicorp/go-azure-helpers/response"
	"github.com/hashicorp/terraform-plugin-sdk/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/helper/validation"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/azure"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/helpers/tf"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/clients"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/features"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/services/mssql/helper"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/tags"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/internal/timeouts"
	"github.com/terraform-providers/terraform-provider-azurerm/azurerm/utils"
)

func resourceArmMsSqlServer() *schema.Resource {
	return &schema.Resource{
		Create: resourceArmMsSqlServerCreateUpdate,
		Read:   resourceArmMsSqlServerRead,
		Update: resourceArmMsSqlServerCreateUpdate,
		Delete: resourceArmMsSqlServerDelete,

		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(60 * time.Minute),
			Read:   schema.DefaultTimeout(5 * time.Minute),
			Update: schema.DefaultTimeout(60 * time.Minute),
			Delete: schema.DefaultTimeout(60 * time.Minute),
		},

		Schema: map[string]*schema.Schema{
			"name": {
				Type:         schema.TypeString,
				Required:     true,
				ForceNew:     true,
				ValidateFunc: azure.ValidateMsSqlServerName,
			},

			"location": azure.SchemaLocation(),

			"resource_group_name": azure.SchemaResourceGroupName(),

			"administrator_login": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
			},

			"administrator_login_password": {
				Type:      schema.TypeString,
				Required:  true,
				Sensitive: true,
			},
			"version": {
				Type:     schema.TypeString,
				Required: true,
				ForceNew: true,
				ValidateFunc: validation.StringInSlice([]string{
					"2.0",
					"12.0",
				}, true),
			},

			"extended_auditing_policy": helper.ExtendedAuditingSchema(),

			"connection_policy": {
				Type:     schema.TypeString,
				Optional: true,
				Default:  string(sql.ServerConnectionTypeDefault),
				ValidateFunc: validation.StringInSlice([]string{
					string(sql.ServerConnectionTypeDefault),
					string(sql.ServerConnectionTypeProxy),
					string(sql.ServerConnectionTypeRedirect),
				}, false),
			},

			"identity": {
				Type:     schema.TypeList,
				Optional: true,
				MaxItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"type": {
							Type:     schema.TypeString,
							Required: true,
							ValidateFunc: validation.StringInSlice([]string{
								"SystemAssigned",
							}, false),
						},
						"principal_id": {
							Type:     schema.TypeString,
							Computed: true,
						},
						"tenant_id": {
							Type:     schema.TypeString,
							Computed: true,
						},
					},
				},
			},

			"public_network_access_enabled": {
				Type:     schema.TypeBool,
				Optional: true,
				Default:  true,
			},

			// exists in SDK however API seems to ignore it: https://github.com/Azure/azure-sdk-for-go/issues/8663
			/*"ssl_minimal_tls_version_enforced": {
				Type:     schema.TypeString,
				Optional: true,
				ValidateFunc: validation.StringInSlice([]string{
					"1.0",
					"1.1",
					"1.2",
				}, false),
			},*/

			"fully_qualified_domain_name": {
				Type:     schema.TypeString,
				Computed: true,
			},

			"tags": tags.Schema(),
		},
	}
}

func resourceArmMsSqlServerCreateUpdate(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).MSSQL.ServersClient
	auditingClient := meta.(*clients.Client).MSSQL.ServerExtendedBlobAuditingPoliciesClient
	connectionClient := meta.(*clients.Client).MSSQL.ServerConnectionPoliciesClient
	ctx, cancel := timeouts.ForCreateUpdate(meta.(*clients.Client).StopContext, d)
	defer cancel()

	name := d.Get("name").(string)
	resGroup := d.Get("resource_group_name").(string)
	location := azure.NormalizeLocation(d.Get("location").(string))
	adminUsername := d.Get("administrator_login").(string)
	version := d.Get("version").(string)

	t := d.Get("tags").(map[string]interface{})
	metadata := tags.Expand(t)

	if features.ShouldResourcesBeImported() && d.IsNewResource() {
		existing, err := client.Get(ctx, resGroup, name)
		if err != nil {
			if !utils.ResponseWasNotFound(existing.Response) {
				return fmt.Errorf("Error checking for presence of existing SQL Server %q (Resource Group %q): %+v", name, resGroup, err)
			}
		}

		if existing.ID != nil && *existing.ID != "" {
			return tf.ImportAsExistsError("azurerm_mssql_server", *existing.ID)
		}
	}

	parameters := sql.Server{
		Location: utils.String(location),
		Tags:     metadata,
		ServerProperties: &sql.ServerProperties{
			Version:             utils.String(version),
			AdministratorLogin:  utils.String(adminUsername),
			PublicNetworkAccess: sql.ServerPublicNetworkAccessEnabled,
		},
	}

	if d.HasChange("administrator_login_password") {
		adminPassword := d.Get("administrator_login_password").(string)
		parameters.ServerProperties.AdministratorLoginPassword = utils.String(adminPassword)
	}

	if v := d.Get("public_network_access_enabled"); !v.(bool) {
		parameters.ServerProperties.PublicNetworkAccess = sql.ServerPublicNetworkAccessDisabled
	}

	if _, ok := d.GetOk("identity"); ok {
		parameters.Identity = expandAzureRmSqlServerIdentity(d)
	}

	future, err := client.CreateOrUpdate(ctx, resGroup, name, parameters)
	if err != nil {
		return fmt.Errorf("Error issuing create/update request for SQL Server %q (Resource Group %q): %+v", name, resGroup, err)
	}

	if err = future.WaitForCompletionRef(ctx, client.Client); err != nil {
		if response.WasConflict(future.Response()) {
			return fmt.Errorf("SQL Server names need to be globally unique and %q is already in use.", name)
		}

		return fmt.Errorf("Error waiting on create/update future for SQL Server %q (Resource Group %q): %+v", name, resGroup, err)
	}

	resp, err := client.Get(ctx, resGroup, name)
	if err != nil {
		return fmt.Errorf("Error issuing get request for SQL Server %q (Resource Group %q): %+v", name, resGroup, err)
	}

	d.SetId(*resp.ID)

	connection := sql.ServerConnectionPolicy{
		ServerConnectionPolicyProperties: &sql.ServerConnectionPolicyProperties{
			ConnectionType: sql.ServerConnectionType(d.Get("connection_policy").(string)),
		},
	}
	if _, err = connectionClient.CreateOrUpdate(ctx, resGroup, name, connection); err != nil {
		return fmt.Errorf("Error issuing create/update request for SQL Server %q Connection Policy (Resource Group %q): %+v", name, resGroup, err)
	}

	auditingProps := sql.ExtendedServerBlobAuditingPolicy{
		ExtendedServerBlobAuditingPolicyProperties: helper.ExpandAzureRmSqlServerBlobAuditingPolicies(d.Get("extended_auditing_policy").([]interface{})),
	}
	if _, err = auditingClient.CreateOrUpdate(ctx, resGroup, name, auditingProps); err != nil {
		return fmt.Errorf("Error issuing create/update request for SQL Server %q Blob Auditing Policies(Resource Group %q): %+v", name, resGroup, err)
	}

	return resourceArmMsSqlServerRead(d, meta)
}

func resourceArmMsSqlServerRead(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).MSSQL.ServersClient
	auditingClient := meta.(*clients.Client).MSSQL.ServerExtendedBlobAuditingPoliciesClient
	connectionClient := meta.(*clients.Client).MSSQL.ServerConnectionPoliciesClient
	ctx, cancel := timeouts.ForRead(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := azure.ParseAzureResourceID(d.Id())
	if err != nil {
		return err
	}

	resGroup := id.ResourceGroup
	name := id.Path["servers"]

	resp, err := client.Get(ctx, resGroup, name)
	if err != nil {
		if utils.ResponseWasNotFound(resp.Response) {
			log.Printf("[INFO] Error reading SQL Server %q - removing from state", d.Id())
			d.SetId("")
			return nil
		}

		return fmt.Errorf("Error reading SQL Server %s: %v", name, err)
	}

	d.Set("name", name)
	d.Set("resource_group_name", resGroup)
	if location := resp.Location; location != nil {
		d.Set("location", azure.NormalizeLocation(*location))
	}

	if err := d.Set("identity", flattenAzureRmSqlServerIdentity(resp.Identity)); err != nil {
		return fmt.Errorf("Error setting `identity`: %+v", err)
	}

	if props := resp.ServerProperties; props != nil {
		d.Set("version", props.Version)
		d.Set("administrator_login", props.AdministratorLogin)
		d.Set("fully_qualified_domain_name", props.FullyQualifiedDomainName)
		d.Set("public_network_access_enabled", props.PublicNetworkAccess == sql.ServerPublicNetworkAccessEnabled)
	}

	connection, err := connectionClient.Get(ctx, resGroup, name)
	if err != nil {
		return fmt.Errorf("Error reading SQL Server %s Blob Connection Policy: %v ", name, err)
	}

	if props := connection.ServerConnectionPolicyProperties; props != nil {
		d.Set("connection_policy", string(props.ConnectionType))
	}

	auditingResp, err := auditingClient.Get(ctx, resGroup, name)
	if err != nil {
		return fmt.Errorf("Error reading SQL Server %s Blob Auditing Policies: %v ", name, err)
	}

	if err := d.Set("extended_auditing_policy", helper.FlattenAzureRmSqlServerBlobAuditingPolicies(&auditingResp, d)); err != nil {
		return fmt.Errorf("Error setting `extended_auditing_policy`: %+v", err)
	}

	return tags.FlattenAndSet(d, resp.Tags)
}

func resourceArmMsSqlServerDelete(d *schema.ResourceData, meta interface{}) error {
	client := meta.(*clients.Client).MSSQL.ServersClient
	ctx, cancel := timeouts.ForDelete(meta.(*clients.Client).StopContext, d)
	defer cancel()

	id, err := azure.ParseAzureResourceID(d.Id())
	if err != nil {
		return err
	}

	resGroup := id.ResourceGroup
	name := id.Path["servers"]

	future, err := client.Delete(ctx, resGroup, name)
	if err != nil {
		return fmt.Errorf("Error deleting SQL Server %s: %+v", name, err)
	}

	return future.WaitForCompletionRef(ctx, client.Client)
}

func expandAzureRmSqlServerIdentity(d *schema.ResourceData) *sql.ResourceIdentity {
	identities := d.Get("identity").([]interface{})
	if len(identities) == 0 {
		return &sql.ResourceIdentity{}
	}
	identity := identities[0].(map[string]interface{})
	identityType := sql.IdentityType(identity["type"].(string))
	return &sql.ResourceIdentity{
		Type: identityType,
	}
}
func flattenAzureRmSqlServerIdentity(identity *sql.ResourceIdentity) []interface{} {
	if identity == nil {
		return []interface{}{}
	}
	result := make(map[string]interface{})
	result["type"] = identity.Type
	if identity.PrincipalID != nil {
		result["principal_id"] = identity.PrincipalID.String()
	}
	if identity.TenantID != nil {
		result["tenant_id"] = identity.TenantID.String()
	}

	return []interface{}{result}
}
