// Copyright 2024
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package sveltos

import (
	"context"
	"fmt"
	"math"
	"strings"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	sveltosv1beta1 "github.com/projectsveltos/addon-controller/api/v1beta1"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	fluxcdmeta "github.com/fluxcd/pkg/apis/meta"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"github.com/Masterminds/semver/v3"

	kcm "github.com/K0rdent/kcm/api/v1beta1"
	"github.com/K0rdent/kcm/internal/utils"
)

const driftIgnorePatch = `- op: add
  path: /metadata/annotations/projectsveltos.io~1driftDetectionIgnore
  value: ok`

type ReconcileProfileOpts struct {
	OwnerReference       *metav1.OwnerReference
	SyncMode             string
	LabelSelector        metav1.LabelSelector
	HelmCharts           []sveltosv1beta1.HelmChart
	KustomizationRefs    []sveltosv1beta1.KustomizationRef
	TemplateResourceRefs []sveltosv1beta1.TemplateResourceRef
	PolicyRefs           []sveltosv1beta1.PolicyRef
	DriftIgnore          []libsveltosv1beta1.PatchSelector
	DriftExclusions      []sveltosv1beta1.DriftExclusion
	Priority             int32
	StopOnConflict       bool
	Reload               bool
	ContinueOnError      bool
}

// ReconcileClusterProfile reconciles a Sveltos ClusterProfile object.
func ReconcileClusterProfile(
	ctx context.Context,
	cl client.Client,
	name string,
	opts ReconcileProfileOpts,
) (*sveltosv1beta1.ClusterProfile, error) {
	l := ctrl.LoggerFrom(ctx)
	obj := objectMeta(opts.OwnerReference)
	obj.SetName(name)

	cp := &sveltosv1beta1.ClusterProfile{
		ObjectMeta: obj,
	}

	operation, err := ctrl.CreateOrUpdate(ctx, cl, cp, func() error {
		spec, err := GetSpec(&opts)
		if err != nil {
			return err
		}
		cp.Spec = *spec

		return nil
	})
	if err != nil {
		return nil, err
	}

	if operation == controllerutil.OperationResultCreated || operation == controllerutil.OperationResultUpdated {
		l.Info("Successfully mutated ClusterProfile", "ClusterProfile", client.ObjectKeyFromObject(cp), "operation_result", operation)
	}

	return cp, nil
}

// ReconcileProfile reconciles a Sveltos Profile object.
func ReconcileProfile(
	ctx context.Context,
	cl client.Client,
	namespace string,
	name string,
	opts ReconcileProfileOpts,
) (*sveltosv1beta1.Profile, error) {
	l := ctrl.LoggerFrom(ctx)
	obj := objectMeta(opts.OwnerReference)
	obj.SetNamespace(namespace)
	obj.SetName(name)

	p := &sveltosv1beta1.Profile{
		ObjectMeta: obj,
	}

	operation, err := ctrl.CreateOrUpdate(ctx, cl, p, func() error {
		spec, err := GetSpec(&opts)
		if err != nil {
			return err
		}
		p.Spec = *spec

		return nil
	})
	if err != nil {
		return nil, err
	}

	if operation == controllerutil.OperationResultCreated || operation == controllerutil.OperationResultUpdated {
		l.Info("Successfully mutated Profile", "Profile", client.ObjectKeyFromObject(p), "operation_result", operation)
	}

	return p, nil
}

// GetHelmCharts returns slice of helm chart options to use with Sveltos.
// Namespace is the namespace of the referred templates in services slice.
func GetHelmCharts(ctx context.Context, c client.Client, namespace string, services []kcm.Service) ([]sveltosv1beta1.HelmChart, error) {
	l := ctrl.LoggerFrom(ctx)
	helmCharts := []sveltosv1beta1.HelmChart{}

	// NOTE: The Profile/ClusterProfile object will be updated with
	// no helm charts if len(mc.Spec.Services) == 0. This will result
	// in the helm charts being uninstalled on matching clusters if
	// Profile/ClusterProfile originally had len(m.Spec.Sevices) > 0.
	for _, svc := range services {
		if svc.Disable {
			l.Info("Skip adding ServiceTemplate", "service_template_name", svc.Template, "is_disabled", svc.Disable)
			continue
		}

		// Here we can use the same namespace for all services
		// because if the services slice is part of:
		// 1. ClusterDeployment: Then the referred template must be in its own namespace.
		// 2. MultiClusterService: Then the referred template must be in system namespace.
		tmpl, err := serviceTemplateObjectFromService(ctx, c, svc, namespace)
		if err != nil {
			return nil, err
		}

		if tmpl.Spec.Helm == nil {
			continue
		}

		if !tmpl.Status.Valid {
			continue
		}

		var helmChart sveltosv1beta1.HelmChart
		switch {
		case tmpl.Spec.Helm.ChartRef != nil, tmpl.Spec.Helm.ChartSpec != nil:
			helmChart, err = helmChartFromSpecOrRef(ctx, c, namespace, svc, tmpl)
		case tmpl.Spec.Helm.ChartSource != nil:
			helmChart, err = helmChartFromFluxSource(ctx, svc, tmpl)
		default:
			return nil, fmt.Errorf("ServiceTemplate %s/%s has no Helm chart defined", tmpl.Namespace, tmpl.Name)
		}

		if err != nil {
			return nil, err
		}

		helmCharts = append(helmCharts, helmChart)
	}

	return helmCharts, nil
}

func helmChartFromSpecOrRef(
	ctx context.Context,
	c client.Client,
	namespace string,
	svc kcm.Service,
	template *kcm.ServiceTemplate,
) (sveltosv1beta1.HelmChart, error) {
	var helmChart sveltosv1beta1.HelmChart
	if template.GetCommonStatus() == nil || template.GetCommonStatus().ChartRef == nil {
		return helmChart, fmt.Errorf("status for ServiceTemplate %s/%s has not been updated yet", template.Namespace, template.Name)
	}
	templateRef := client.ObjectKeyFromObject(template)
	chart := &sourcev1.HelmChart{}
	chartRef := client.ObjectKey{
		Namespace: template.GetCommonStatus().ChartRef.Namespace,
		Name:      template.GetCommonStatus().ChartRef.Name,
	}
	if err := c.Get(ctx, chartRef, chart); err != nil {
		return helmChart, fmt.Errorf("failed to get HelmChart %s referenced by ServiceTemplate %s: %w", chartRef.String(), templateRef.String(), err)
	}

	chartVersion := ""
	if chart.Status.Artifact != nil && chart.Status.Artifact.Revision != "" {
	    if _, err := semver.NewVersion(chart.Status.Artifact.Revision); err == nil {
		// Try to get the HelmChart version from status.artifact.revision
		// It contains the valid chart version for charts from a GitRepository
		chartVersion = chart.Status.Artifact.Revision
	    } else {
		// Fallback to HelmChart version from spec.version if has the valid format
		_, err := semver.NewVersion(chart.Spec.Version)
		if err != nil {
		    return helmChart, fmt.Errorf("failed to determine version of HelmChart %s referenced by ServiceTemplate %s: %w", chartRef.String(), templateRef.String(), err)
		}
		chartVersion = chart.Spec.Version
	    }
	}

	repoRef := client.ObjectKey{
		// Using chart's namespace because it's source
		// should be within the same namespace.
		Namespace: chart.Namespace,
		Name:      chart.Spec.SourceRef.Name,
	}

	var repoUrl string
	var repoChartName string
	var RegistryCredentialsConfig *sveltosv1beta1.RegistryCredentialsConfig
	chartName := chart.Spec.Chart

	switch chart.Spec.SourceRef.Kind {
	    case sourcev1.HelmRepositoryKind:
	        repo := &sourcev1.HelmRepository{}
		if err := c.Get(ctx, repoRef, repo); err != nil {
		    return helmChart, fmt.Errorf("failed to get %s: %w", repoRef.String(), err)
		}
	        repoUrl = repo.Spec.URL
		repoChartName = func() string {
			if repo.Spec.Type == utils.RegistryTypeOCI {
				return chartName
			}
			// Sveltos accepts ChartName in <repository>/<chart> format for non-OCI.
			// We don't have a repository name, so we can use <chart>/<chart> instead.
			// See: https://projectsveltos.github.io/sveltos/addons/helm_charts/.
			return fmt.Sprintf("%s/%s", chartName, chartName)
		}()
	        RegistryCredentialsConfig = generateRegistryCredentialsConfig(namespace, repo.Spec.Insecure, repo.Spec.SecretRef)
	    case sourcev1.GitRepositoryKind:
	        repo := &sourcev1.GitRepository{}
		if err := c.Get(ctx, repoRef, repo); err != nil {
		    return helmChart, fmt.Errorf("failed to get %s: %w", repoRef.String(), err)
		}
		repoUrl = fmt.Sprintf("gitrepository://%s/%s/%s", chart.Namespace, chart.Spec.SourceRef.Name, chartName)
		// Sveltos accepts ChartName in <repository>/<chart> format for non-OCI.
		// We don't have a repository name, so we can use <chart>/<chart> instead.
		// See: https://projectsveltos.github.io/sveltos/addons/helm_charts/.
		repoChartName = chartName
	        RegistryCredentialsConfig = generateRegistryCredentialsConfig(namespace, false, repo.Spec.SecretRef)
	    default:
	        return helmChart, fmt.Errorf("Unsupported HelmChart source kind %s", repoRef.String())
	}

	helmChart = sveltosv1beta1.HelmChart{
		Values:        svc.Values,
		ValuesFrom:    svc.ValuesFrom,
		RepositoryURL: repoUrl,
		// We don't have repository name so chart name becomes repository name.
		RepositoryName: chartName,
		ChartName: repoChartName,
		ChartVersion: chartVersion,
		ReleaseName:  svc.Name,
		ReleaseNamespace: func() string {
			if svc.Namespace != "" {
				return svc.Namespace
			}
			return svc.Name
		}(),
		RegistryCredentialsConfig: RegistryCredentialsConfig,
	}
	return helmChart, nil
}

func generateRegistryCredentialsConfig(namespace string, insecure bool, secretRef *fluxcdmeta.LocalObjectReference) *sveltosv1beta1.RegistryCredentialsConfig {
	if !insecure && secretRef == nil {
		return nil
	}

	c := new(sveltosv1beta1.RegistryCredentialsConfig)

	// The reason it is passed to PlainHTTP instead of InsecureSkipTLSVerify is because
	// the source.Spec.Insecure field is meant to be used for connecting to repositories
	// over plain HTTP, which is different than what InsecureSkipTLSVerify is meant for.
	// See: https://github.com/fluxcd/source-controller/pull/1288
	c.PlainHTTP = insecure
	if c.PlainHTTP {
		// InsecureSkipTLSVerify is redundant in this case.
		// At the time of implementation, Sveltos would return an error when PlainHTTP
		// and InsecureSkipTLSVerify were both set, so verify before removing.
		c.InsecureSkipTLSVerify = false
	}

	if secretRef != nil {
		c.CredentialsSecretRef = &corev1.SecretReference{
			Name:      secretRef.Name,
			Namespace: namespace,
		}
	}

	return c
}

func helmChartFromFluxSource(
	_ context.Context,
	svc kcm.Service,
	template *kcm.ServiceTemplate,
) (sveltosv1beta1.HelmChart, error) {
	var helmChart sveltosv1beta1.HelmChart
	if template.Status.SourceStatus == nil {
		return helmChart, fmt.Errorf("status for ServiceTemplate %s/%s has not been updated yet", template.Namespace, template.Name)
	}

	source := template.Spec.Helm.ChartSource
	status := template.Status.SourceStatus
	sanitizedPath := strings.TrimPrefix(strings.TrimPrefix(source.Path, "."), "/")
	url := fmt.Sprintf("%s://%s/%s/%s", status.Kind, status.Namespace, status.Name, sanitizedPath)

	helmChart = sveltosv1beta1.HelmChart{
		RepositoryURL:    url,
		ReleaseName:      svc.Name,
		ReleaseNamespace: svc.Namespace,
		Values:           svc.Values,
		ValuesFrom:       svc.ValuesFrom,
	}

	return helmChart, nil
}

// GetKustomizationRefs returns a list of KustomizationRefs for the given services.
func GetKustomizationRefs(ctx context.Context, c client.Client, namespace string, services []kcm.Service) ([]sveltosv1beta1.KustomizationRef, error) {
	l := ctrl.LoggerFrom(ctx)
	kustomizationRefs := []sveltosv1beta1.KustomizationRef{}

	for _, svc := range services {
		if svc.Disable {
			l.Info("Skip adding ServiceTemplate", "service_template_name", svc.Template, "is_disabled", svc.Disable)
			continue
		}

		// Here we can use the same namespace for all services
		// because if the services slice is part of:
		// 1. ClusterDeployment: Then the referred template must be in its own namespace.
		// 2. MultiClusterService: Then the referred template must be in system namespace.
		tmpl, err := serviceTemplateObjectFromService(ctx, c, svc, namespace)
		if err != nil {
			return nil, err
		}

		if tmpl.Spec.Kustomize == nil {
			continue
		}

		if !tmpl.Status.Valid {
			continue
		}

		kustomization := sveltosv1beta1.KustomizationRef{
			Namespace:       tmpl.Status.SourceStatus.Namespace,
			Name:            tmpl.Status.SourceStatus.Name,
			Kind:            tmpl.Status.SourceStatus.Kind,
			Path:            tmpl.Spec.Kustomize.Path,
			TargetNamespace: svc.Namespace,
			DeploymentType:  sveltosv1beta1.DeploymentType(tmpl.Spec.Kustomize.DeploymentType),
			// Values:          svc.Values,
			ValuesFrom: svc.ValuesFrom,
		}

		kustomizationRefs = append(kustomizationRefs, kustomization)
	}
	return kustomizationRefs, nil
}

// GetPolicyRefs returns a list of PolicyRefs for the given services.
func GetPolicyRefs(ctx context.Context, c client.Client, namespace string, services []kcm.Service) ([]sveltosv1beta1.PolicyRef, error) {
	l := ctrl.LoggerFrom(ctx)
	policyRefs := []sveltosv1beta1.PolicyRef{}

	for _, svc := range services {
		if svc.Disable {
			l.Info("Skip adding ServiceTemplate", "service_template_name", svc.Template, "is_disabled", svc.Disable)
			continue
		}

		// Here we can use the same namespace for all services
		// because if the services slice is part of:
		// 1. ClusterDeployment: Then the referred template must be in its own namespace.
		// 2. MultiClusterService: Then the referred template must be in system namespace.
		tmpl, err := serviceTemplateObjectFromService(ctx, c, svc, namespace)
		if err != nil {
			return nil, err
		}

		if tmpl.Spec.Resources == nil {
			continue
		}

		if !tmpl.Status.Valid {
			continue
		}

		policyRef := sveltosv1beta1.PolicyRef{
			Namespace:      tmpl.Status.SourceStatus.Namespace,
			Name:           tmpl.Status.SourceStatus.Name,
			Kind:           tmpl.Status.SourceStatus.Kind,
			Path:           tmpl.Spec.Resources.Path,
			DeploymentType: sveltosv1beta1.DeploymentType(tmpl.Spec.Resources.DeploymentType),
		}

		policyRefs = append(policyRefs, policyRef)
	}
	return policyRefs, nil
}

// GetSpec returns a spec object to be used with
// a Sveltos Profile or ClusterProfile object.
func GetSpec(opts *ReconcileProfileOpts) (*sveltosv1beta1.Spec, error) {
	tier, err := priorityToTier(opts.Priority)
	if err != nil {
		return nil, err
	}

	spec := &sveltosv1beta1.Spec{
		ClusterSelector: libsveltosv1beta1.Selector{
			LabelSelector: opts.LabelSelector,
		},
		Tier:                 tier,
		ContinueOnConflict:   !opts.StopOnConflict,
		HelmCharts:           opts.HelmCharts,
		Reloader:             opts.Reload,
		SyncMode:             sveltosv1beta1.SyncMode(opts.SyncMode),
		TemplateResourceRefs: opts.TemplateResourceRefs,
		KustomizationRefs:    opts.KustomizationRefs,
		PolicyRefs:           opts.PolicyRefs,
		DriftExclusions:      opts.DriftExclusions,
		ContinueOnError:      opts.ContinueOnError,
	}

	for _, target := range opts.DriftIgnore {
		spec.Patches = append(spec.Patches, libsveltosv1beta1.Patch{
			Target: &target,
			Patch:  driftIgnorePatch,
		})
	}

	return spec, nil
}

func objectMeta(owner *metav1.OwnerReference) metav1.ObjectMeta {
	obj := metav1.ObjectMeta{
		Labels: map[string]string{
			kcm.KCMManagedLabelKey: kcm.KCMManagedLabelValue,
		},
	}

	if owner != nil {
		obj.OwnerReferences = []metav1.OwnerReference{*owner}
	}

	return obj
}

// DeleteProfile deletes a Sveltos Profile object.
func DeleteProfile(ctx context.Context, cl client.Client, namespace, name string) error {
	err := cl.Delete(ctx, &sveltosv1beta1.Profile{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
		},
	})

	return client.IgnoreNotFound(err)
}

// DeleteClusterProfile deletes a Sveltos ClusterProfile object.
func DeleteClusterProfile(ctx context.Context, cl client.Client, name string) error {
	err := cl.Delete(ctx, &sveltosv1beta1.ClusterProfile{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
	})

	return client.IgnoreNotFound(err)
}

// priorityToTier converts priority value to Sveltos tier value.
func priorityToTier(priority int32) (int32, error) {
	var mini int32 = 1
	maxi := math.MaxInt32 - mini

	// This check is needed because Sveltos asserts a min value of 1 on tier.
	if priority >= mini && priority <= maxi {
		return math.MaxInt32 - priority, nil
	}

	return 0, fmt.Errorf("invalid value %d, priority has to be between %d and %d", priority, mini, maxi)
}

// serviceTemplateObjectFromService returns the [github.com/K0rdent/kcm/api/v1beta1.ServiceTemplate]
// object found either by direct reference or in [github.com/K0rdent/kcm/api/v1beta1.ServiceTemplateChain] by defined version.
func serviceTemplateObjectFromService(
	ctx context.Context,
	cl client.Client,
	svc kcm.Service,
	namespace string,
) (*kcm.ServiceTemplate, error) {
	template := new(kcm.ServiceTemplate)
	key := client.ObjectKey{Name: svc.Template, Namespace: namespace}
	if err := cl.Get(ctx, key, template); err != nil {
		return nil, fmt.Errorf("failed to get ServiceTemplate %s: %w", key.String(), err)
	}

	if svc.TemplateChain != "" {
		templateChain := new(kcm.ServiceTemplateChain)
		key := client.ObjectKey{Name: svc.TemplateChain, Namespace: namespace}
		if err := cl.Get(ctx, key, templateChain); err != nil {
			return nil, fmt.Errorf("failed to get ServiceTemplateChain %s: %w", key.String(), err)
		}

		if !templateChain.Status.Valid {
			return nil, fmt.Errorf("the ServiceTemplateChain %s is invalid with the error: %s", key, templateChain.Status.ValidationError)
		}

		matchingTemplateFound := false
		for _, supportedTemplate := range templateChain.Spec.SupportedTemplates {
			if supportedTemplate.Name != svc.Template {
				continue
			}
			template = new(kcm.ServiceTemplate)
			templateKey := client.ObjectKey{Name: supportedTemplate.Name, Namespace: namespace}
			if err := cl.Get(ctx, templateKey, template); err != nil {
				return nil, fmt.Errorf("failed to get ServiceTemplate %s: %w", key.String(), err)
			}
			matchingTemplateFound = true
		}
		if !matchingTemplateFound {
			return nil, fmt.Errorf("ServiceTemplate %s is not supported by ServiceTemplateChain %s", svc.Template, key)
		}
	}

	return template, nil
}
