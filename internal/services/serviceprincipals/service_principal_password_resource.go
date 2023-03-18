package serviceprincipals

import (
	"context"
	"encoding/base64"
	"errors"
	log "github.com/sourcegraph-ce/logrus"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/go-azure-sdk/sdk/odata"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/validation"
	"github.com/hashicorp/terraform-provider-azuread/internal/clients"
	"github.com/hashicorp/terraform-provider-azuread/internal/helpers"
	"github.com/hashicorp/terraform-provider-azuread/internal/services/serviceprincipals/migrations"
	"github.com/hashicorp/terraform-provider-azuread/internal/services/serviceprincipals/parse"
	"github.com/hashicorp/terraform-provider-azuread/internal/tf"
	"github.com/hashicorp/terraform-provider-azuread/internal/utils"
	"github.com/hashicorp/terraform-provider-azuread/internal/validate"
)

func servicePrincipalPasswordResource() *schema.Resource {
	return &schema.Resource{
		CreateContext: servicePrincipalPasswordResourceCreate,
		ReadContext:   servicePrincipalPasswordResourceRead,
		DeleteContext: servicePrincipalPasswordResourceDelete,

		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(5 * time.Minute),
			Read:   schema.DefaultTimeout(5 * time.Minute),
			Update: schema.DefaultTimeout(5 * time.Minute),
			Delete: schema.DefaultTimeout(5 * time.Minute),
		},

		SchemaVersion: 1,
		StateUpgraders: []schema.StateUpgrader{
			{
				Type:    migrations.ResourceServicePrincipalPasswordInstanceResourceV0().CoreConfigSchema().ImpliedType(),
				Upgrade: migrations.ResourceServicePrincipalPasswordInstanceStateUpgradeV0,
				Version: 0,
			},
		},

		Schema: map[string]*schema.Schema{
			"service_principal_id": {
				Description:      "The object ID of the service principal for which this password should be created",
				Type:             schema.TypeString,
				Required:         true,
				ForceNew:         true,
				ValidateDiagFunc: validate.UUID,
			},

			"display_name": {
				Description: "A display name for the password",
				Type:        schema.TypeString,
				Optional:    true,
				Computed:    true,
				ForceNew:    true,
			},

			"start_date": {
				Description:  "The start date from which the password is valid, formatted as an RFC3339 date string (e.g. `2018-01-01T01:02:03Z`). If this isn't specified, the current date is used",
				Type:         schema.TypeString,
				Optional:     true,
				Computed:     true,
				ForceNew:     true,
				ValidateFunc: validation.IsRFC3339Time,
			},

			"end_date": {
				Description:   "The end date until which the password is valid, formatted as an RFC3339 date string (e.g. `2018-01-01T01:02:03Z`)",
				Type:          schema.TypeString,
				Optional:      true,
				Computed:      true,
				ForceNew:      true,
				ConflictsWith: []string{"end_date_relative"},
				ValidateFunc:  validation.IsRFC3339Time,
			},

			"end_date_relative": {
				Description:      "A relative duration for which the password is valid until, for example `240h` (10 days) or `2400h30m`. Changing this field forces a new resource to be created",
				Type:             schema.TypeString,
				Optional:         true,
				ForceNew:         true,
				ConflictsWith:    []string{"end_date"},
				ValidateDiagFunc: validate.NoEmptyStrings,
			},

			"rotate_when_changed": {
				Description: "Arbitrary map of values that, when changed, will trigger rotation of the password",
				Type:        schema.TypeMap,
				Optional:    true,
				ForceNew:    true,
				Elem: &schema.Schema{
					Type: schema.TypeString,
				},
			},

			"key_id": {
				Description: "A UUID used to uniquely identify this password credential",
				Type:        schema.TypeString,
				Computed:    true,
			},

			"value": {
				Description: "The password for this service principal, which is generated by Azure Active Directory",
				Type:        schema.TypeString,
				Computed:    true,
				Sensitive:   true,
			},
		},
	}
}

func servicePrincipalPasswordResourceCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*clients.Client).ServicePrincipals.ServicePrincipalsClient
	objectId := d.Get("service_principal_id").(string)

	credential, err := helpers.PasswordCredentialForResource(d)
	if err != nil {
		attr := ""
		if kerr, ok := err.(helpers.CredentialError); ok {
			attr = kerr.Attr()
		}
		return tf.ErrorDiagPathF(err, attr, "Generating password credentials for service principal with object ID %q", objectId)
	}
	if credential == nil {
		return tf.ErrorDiagF(errors.New("nil credential was returned"), "Generating password credentials for service principal with object ID %q", objectId)
	}

	tf.LockByName(servicePrincipalResourceName, objectId)
	defer tf.UnlockByName(servicePrincipalResourceName, objectId)

	sp, status, err := client.Get(ctx, objectId, odata.Query{})
	if err != nil {
		if status == http.StatusNotFound {
			return tf.ErrorDiagPathF(nil, "service_principal_id", "Service principal with object ID %q was not found", objectId)
		}
		return tf.ErrorDiagPathF(err, "service_principal_id", "Retrieving service principal with object ID %q", objectId)
	}
	if sp == nil || sp.ID() == nil {
		return tf.ErrorDiagF(errors.New("nil service principal or service principal with nil ID was returned"), "API error retrieving service principal with object ID %q", objectId)
	}

	newCredential, _, err := client.AddPassword(ctx, *sp.ID(), *credential)
	if err != nil {
		return tf.ErrorDiagF(err, "Adding password for service principal with object ID %q", *sp.ID())
	}
	if newCredential == nil {
		return tf.ErrorDiagF(errors.New("nil credential received when adding password"), "API error adding password for service principal with object ID %q", *sp.ID())
	}
	if newCredential.KeyId == nil {
		return tf.ErrorDiagF(errors.New("nil or empty keyId received"), "API error adding password for service principal with object ID %q", *sp.ID())
	}
	if newCredential.SecretText == nil || len(*newCredential.SecretText) == 0 {
		return tf.ErrorDiagF(errors.New("nil or empty password received"), "API error adding password for service principal with object ID %q", *sp.ID())
	}

	id := parse.NewCredentialID(*sp.ID(), "password", *newCredential.KeyId)

	// Wait for the credential to appear in the service principal manifest, this can take several minutes
	timeout, _ := ctx.Deadline()
	polledForCredential, err := (&resource.StateChangeConf{
		Pending:                   []string{"Waiting"},
		Target:                    []string{"Done"},
		Timeout:                   time.Until(timeout),
		MinTimeout:                1 * time.Second,
		ContinuousTargetOccurence: 5,
		Refresh: func() (interface{}, string, error) {
			servicePrincipal, _, err := client.Get(ctx, id.ObjectId, odata.Query{})
			if err != nil {
				return nil, "Error", err
			}

			if servicePrincipal.PasswordCredentials != nil {
				for _, cred := range *servicePrincipal.PasswordCredentials {
					if cred.KeyId != nil && strings.EqualFold(*cred.KeyId, id.KeyId) {
						return &cred, "Done", nil
					}
				}
			}

			return nil, "Waiting", nil
		},
	}).WaitForStateContext(ctx)

	if err != nil {
		return tf.ErrorDiagF(err, "Waiting for password credential for service principal with object ID %q", id.ObjectId)
	} else if polledForCredential == nil {
		return tf.ErrorDiagF(errors.New("password credential not found in service principal manifest"), "Waiting for password credential for service principal with object ID %q", id.ObjectId)
	}

	d.SetId(id.String())
	d.Set("value", newCredential.SecretText)

	return servicePrincipalPasswordResourceRead(ctx, d, meta)
}

func servicePrincipalPasswordResourceRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*clients.Client).ServicePrincipals.ServicePrincipalsClient

	id, err := parse.PasswordID(d.Id())
	if err != nil {
		return tf.ErrorDiagPathF(err, "id", "Parsing password credential with ID %q", d.Id())
	}

	servicePrincipal, status, err := client.Get(ctx, id.ObjectId, odata.Query{})
	if err != nil {
		if status == http.StatusNotFound {
			log.Printf("[DEBUG] Service Principal with ID %q for %s credential %q was not found - removing from state!", id.ObjectId, id.KeyType, id.KeyId)
			d.SetId("")
			return nil
		}
		return tf.ErrorDiagPathF(err, "service_principal_id", "Retrieving service principal with object ID %q", id.ObjectId)
	}

	credential := helpers.GetPasswordCredential(servicePrincipal.PasswordCredentials, id.KeyId)
	if credential == nil {
		log.Printf("[DEBUG] Password credential %q (ID %q) was not found - removing from state!", id.KeyId, id.ObjectId)
		d.SetId("")
		return nil
	}

	if credential.DisplayName != nil {
		tf.Set(d, "display_name", credential.DisplayName)
	} else if credential.CustomKeyIdentifier != nil {
		displayName, err := base64.StdEncoding.DecodeString(*credential.CustomKeyIdentifier)
		if err != nil {
			return tf.ErrorDiagPathF(err, "display_name", "Parsing CustomKeyIdentifier")
		}
		tf.Set(d, "display_name", string(displayName))
	}

	tf.Set(d, "key_id", id.KeyId)
	tf.Set(d, "service_principal_id", id.ObjectId)

	startDate := ""
	if v := credential.StartDateTime; v != nil {
		startDate = v.Format(time.RFC3339)
	}
	tf.Set(d, "start_date", startDate)

	endDate := ""
	if v := credential.EndDateTime; v != nil {
		endDate = v.Format(time.RFC3339)
	}
	tf.Set(d, "end_date", endDate)

	return nil
}

func servicePrincipalPasswordResourceDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	client := meta.(*clients.Client).ServicePrincipals.ServicePrincipalsClient

	id, err := parse.PasswordID(d.Id())
	if err != nil {
		return tf.ErrorDiagPathF(err, "id", "Parsing password credential with ID %q", d.Id())
	}

	tf.LockByName(servicePrincipalResourceName, id.ObjectId)
	defer tf.UnlockByName(servicePrincipalResourceName, id.ObjectId)

	if _, err := client.RemovePassword(ctx, id.ObjectId, id.KeyId); err != nil {
		return tf.ErrorDiagF(err, "Removing password credential %q from service principal with object ID %q", id.KeyId, id.ObjectId)
	}

	// Wait for service principal password to be deleted
	if err := helpers.WaitForDeletion(ctx, func(ctx context.Context) (*bool, error) {
		client.BaseClient.DisableRetries = true

		servicePrincipal, _, err := client.Get(ctx, id.ObjectId, odata.Query{})
		if err != nil {
			return nil, err
		}

		credential := helpers.GetPasswordCredential(servicePrincipal.PasswordCredentials, id.KeyId)
		if credential == nil {
			return utils.Bool(false), nil
		}

		return utils.Bool(true), nil
	}); err != nil {
		return tf.ErrorDiagF(err, "Waiting for deletion of password credential %q from service principal with object ID %q", id.KeyId, id.ObjectId)
	}

	return nil
}
