package argocd

import (
	"context"
	"fmt"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	applicationClient "github.com/dcoppa/argo-cd/v2/pkg/apiclient/application"
	application "github.com/dcoppa/argo-cd/v2/pkg/apis/application/v1alpha1"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/retry"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/oboukili/terraform-provider-argocd/internal/features"
	"github.com/oboukili/terraform-provider-argocd/internal/provider"
)

func resourceArgoCDApplication() *schema.Resource {
	return &schema.Resource{
		Description:   "Manages [applications](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/#applications) within ArgoCD.",
		CreateContext: resourceArgoCDApplicationCreate,
		ReadContext:   resourceArgoCDApplicationRead,
		UpdateContext: resourceArgoCDApplicationUpdate,
		DeleteContext: resourceArgoCDApplicationDelete,
		Importer: &schema.ResourceImporter{
			StateContext: schema.ImportStatePassthroughContext,
		},
		Schema: map[string]*schema.Schema{
			"metadata": metadataSchema("applications.argoproj.io"),
			"spec":     applicationSpecSchemaV4(false),
			"cascade": {
				Type:        schema.TypeBool,
				Description: "Whether to applying cascading deletion when application is removed.",
				Optional:    true,
				Default:     true,
			},
			"status": applicationStatusSchema(),
		},
		SchemaVersion: 4,
		StateUpgraders: []schema.StateUpgrader{
			{
				Type:    resourceArgoCDApplicationV0().CoreConfigSchema().ImpliedType(),
				Upgrade: resourceArgoCDApplicationStateUpgradeV0,
				Version: 0,
			},
			{
				Type:    resourceArgoCDApplicationV1().CoreConfigSchema().ImpliedType(),
				Upgrade: resourceArgoCDApplicationStateUpgradeV1,
				Version: 1,
			},
			{
				Type:    resourceArgoCDApplicationV2().CoreConfigSchema().ImpliedType(),
				Upgrade: resourceArgoCDApplicationStateUpgradeV2,
				Version: 2,
			},
			{
				Type:    resourceArgoCDApplicationV3().CoreConfigSchema().ImpliedType(),
				Upgrade: resourceArgoCDApplicationStateUpgradeV3,
				Version: 3,
			},
		},
		Timeouts: &schema.ResourceTimeout{
			Create: schema.DefaultTimeout(5 * time.Minute),
			Update: schema.DefaultTimeout(5 * time.Minute),
			Delete: schema.DefaultTimeout(5 * time.Minute),
		},
	}
}

func resourceArgoCDApplicationCreate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	objectMeta, spec, err := expandApplication(d)
	if err != nil {
		return errorToDiagnostics("failed to expand application", err)
	}

	si := meta.(*provider.ServerInterface)
	if diags := si.InitClients(ctx); diags != nil {
		return pluginSDKDiags(diags)
	}

	apps, err := si.ApplicationClient.List(ctx, &applicationClient.ApplicationQuery{
		Name:         &objectMeta.Name,
		AppNamespace: &objectMeta.Namespace,
	})
	if err != nil && !strings.Contains(err.Error(), "NotFound") {
		return errorToDiagnostics(fmt.Sprintf("failed to list existing applications when creating application %s", objectMeta.Name), err)
	}

	if apps != nil {
		l := len(apps.Items)

		switch {
		case l < 1:
			break
		case l == 1:
			switch apps.Items[0].DeletionTimestamp {
			case nil:
			default:
				// Pre-existing app is still in Kubernetes soft deletion queue
				time.Sleep(time.Duration(*apps.Items[0].DeletionGracePeriodSeconds))
			}
		case l > 1:
			return []diag.Diagnostic{
				{
					Severity: diag.Error,
					Summary:  fmt.Sprintf("found multiple applications matching name '%s' and namespace '%s'", objectMeta.Name, objectMeta.Namespace),
				},
			}
		}
	}

	l := len(spec.Sources)

	switch {
	case l == 1:
		spec.Source = &spec.Sources[0]
		spec.Sources = nil
	case l > 1 && !si.IsFeatureSupported(features.MultipleApplicationSources):
		return featureNotSupported(features.MultipleApplicationSources)
	}

	if spec.SyncPolicy != nil && spec.SyncPolicy.ManagedNamespaceMetadata != nil && !si.IsFeatureSupported(features.ManagedNamespaceMetadata) {
		return featureNotSupported(features.ManagedNamespaceMetadata)
	}

	app, err := si.ApplicationClient.Create(ctx, &applicationClient.ApplicationCreateRequest{
		Application: &application.Application{
			ObjectMeta: objectMeta,
			Spec:       spec,
			TypeMeta: metav1.TypeMeta{
				Kind:       "Application",
				APIVersion: "argoproj.io/v1alpha1",
			},
		},
	})

	if err != nil {
		return argoCDAPIError("create", "application", objectMeta.Name, err)
	} else if app == nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  fmt.Sprintf("application %s could not be created: unknown reason", objectMeta.Name),
			},
		}
	}

	d.SetId(fmt.Sprintf("%s:%s", app.Name, objectMeta.Namespace))

	return resourceArgoCDApplicationFakeRead(ctx, d, meta)
}

func resourceArgoCDApplicationFakeRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	return nil
}

func resourceArgoCDApplicationRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	si := meta.(*provider.ServerInterface)
	if diags := si.InitClients(ctx); diags != nil {
		return pluginSDKDiags(diags)
	}

	ids := strings.Split(d.Id(), ":")
	appName := ids[0]
	namespace := ids[1]

	apps, err := si.ApplicationClient.List(ctx, &applicationClient.ApplicationQuery{
		Name:         &appName,
		AppNamespace: &namespace,
	})
	if err != nil {
		if strings.Contains(err.Error(), "NotFound") {
			d.SetId("")
			return diag.Diagnostics{}
		}

		return argoCDAPIError("read", "application", appName, err)
	}

	l := len(apps.Items)

	switch {
	case l < 1:
		d.SetId("")
		return diag.Diagnostics{}
	case l == 1:
		break
	case l > 1:
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  fmt.Sprintf("found multiple applications matching name '%s' and namespace '%s'", appName, namespace),
			},
		}
	}

	err = flattenApplication(&apps.Items[0], d)
	if err != nil {
		return errorToDiagnostics(fmt.Sprintf("failed to flatten application %s", appName), err)
	}

	return nil
}

func resourceArgoCDApplicationUpdate(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	if ok := d.HasChanges("metadata", "spec"); !ok {
		return resourceArgoCDApplicationRead(ctx, d, meta)
	}

	si := meta.(*provider.ServerInterface)
	if diags := si.InitClients(ctx); diags != nil {
		return pluginSDKDiags(diags)
	}

	ids := strings.Split(d.Id(), ":")
	appQuery := &applicationClient.ApplicationQuery{
		Name:         &ids[0],
		AppNamespace: &ids[1],
	}

	objectMeta, spec, err := expandApplication(d)
	if err != nil {
		return errorToDiagnostics(fmt.Sprintf("failed to expand application %s", *appQuery.Name), err)
	}

	l := len(spec.Sources)

	switch {
	case l == 1:
		spec.Source = &spec.Sources[0]
		spec.Sources = nil
	case l > 1 && !si.IsFeatureSupported(features.MultipleApplicationSources):
		return featureNotSupported(features.MultipleApplicationSources)
	}

	if spec.SyncPolicy != nil && spec.SyncPolicy.ManagedNamespaceMetadata != nil && !si.IsFeatureSupported(features.ManagedNamespaceMetadata) {
		return featureNotSupported(features.ManagedNamespaceMetadata)
	}

	apps, err := si.ApplicationClient.List(ctx, appQuery)
	if err != nil {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  "failed to get application",
				Detail:   err.Error(),
			},
		}
	}

	if len(apps.Items) > 1 {
		return []diag.Diagnostic{
			{
				Severity: diag.Error,
				Summary:  fmt.Sprintf("found multiple applications matching name '%s' and namespace '%s'", *appQuery.Name, *appQuery.AppNamespace),
				Detail:   err.Error(),
			},
		}
	}

	_, err = si.ApplicationClient.Update(ctx, &applicationClient.ApplicationUpdateRequest{
		Application: &application.Application{
			ObjectMeta: objectMeta,
			Spec:       spec,
			TypeMeta: metav1.TypeMeta{
				Kind:       "Application",
				APIVersion: "argoproj.io/v1alpha1",
			},
		},
	})

	if err != nil {
		return argoCDAPIError("update", "application", objectMeta.Name, err)
	}

	time.Sleep(60 * time.Second)

	return resourceArgoCDApplicationRead(ctx, d, meta)
}

func resourceArgoCDApplicationDelete(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	si := meta.(*provider.ServerInterface)
	if diags := si.InitClients(ctx); diags != nil {
		return pluginSDKDiags(diags)
	}

	ids := strings.Split(d.Id(), ":")
	appName := ids[0]
	namespace := ids[1]
	cascade := d.Get("cascade").(bool)

	_, err := si.ApplicationClient.Delete(ctx, &applicationClient.ApplicationDeleteRequest{
		Name:         &appName,
		Cascade:      &cascade,
		AppNamespace: &namespace,
	})

	if err != nil && !strings.Contains(err.Error(), "NotFound") {
		return argoCDAPIError("delete", "application", appName, err)
	}

	_ = retry.RetryContext(ctx, 1*time.Minute, func() *retry.RetryError {
		apps, err := si.ApplicationClient.List(ctx, &applicationClient.ApplicationQuery{
			Name:         &appName,
			AppNamespace: &namespace,
		})

		switch err {
		case nil:
			if apps != nil && len(apps.Items) > 0 {
				return retry.RetryableError(fmt.Errorf("application %s is still present", appName))
			}
		default:
			if !strings.Contains(err.Error(), "NotFound") {
				return retry.NonRetryableError(err)
			}
		}

		d.SetId("")

		return nil
	})

	d.SetId("")

	return nil
}
