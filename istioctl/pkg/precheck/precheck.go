// Copyright © 2021 NAME HERE <EMAIL ADDRESS>
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

package precheck

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"
	authorizationapi "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	crd "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networking "istio.io/api/networking/v1alpha3"
	"istio.io/istio/istioctl/pkg/cli"
	"istio.io/istio/istioctl/pkg/clioptions"
	"istio.io/istio/istioctl/pkg/install/k8sversion"
	"istio.io/istio/istioctl/pkg/util/formatting"
	"istio.io/istio/pkg/config/analysis"
	"istio.io/istio/pkg/config/analysis/analyzers/maturity"
	"istio.io/istio/pkg/config/analysis/diag"
	"istio.io/istio/pkg/config/analysis/local"
	"istio.io/istio/pkg/config/analysis/msg"
	kube3 "istio.io/istio/pkg/config/legacy/source/kube"
	"istio.io/istio/pkg/config/resource"
	"istio.io/istio/pkg/config/schema/gvk"
	"istio.io/istio/pkg/config/schema/kubetypes"
	"istio.io/istio/pkg/kube"
	"istio.io/istio/pkg/kube/controllers"
	"istio.io/istio/pkg/url"
	"istio.io/istio/pkg/util/sets"
)

func Cmd(ctx cli.Context) *cobra.Command {
	var opts clioptions.ControlPlaneOptions
	var skipControlPlane bool
	outputThreshold := formatting.MessageThreshold{Level: diag.Warning}
	var msgOutputFormat string
	var fromCompatibilityVersion string
	// cmd represents the upgradeCheck command
	cmd := &cobra.Command{
		Use:   "precheck",
		Short: "Check whether Istio can safely be installed or upgraded",
		Long:  `precheck inspects a Kubernetes cluster for Istio install and upgrade requirements.`,
		Example: `  # Verify that Istio can be installed or upgraded
  istioctl x precheck

  # Check only a single namespace
  istioctl x precheck --namespace default

  # Check for behavioral changes since a specific version
  istioctl x precheck --from-version 1.10`,
		RunE: func(cmd *cobra.Command, args []string) (err error) {
			msgs := diag.Messages{}
			if !skipControlPlane {
				msgs, err = checkControlPlane(ctx)
				if err != nil {
					return err
				}
			}

			if fromCompatibilityVersion != "" {
				m, err := checkFromVersion(ctx, opts.Revision, fromCompatibilityVersion)
				if err != nil {
					return err
				}
				msgs = append(msgs, m...)
			}

			// Print all the messages to stdout in the specified format
			msgs = msgs.SortedDedupedCopy()
			outputMsgs := diag.Messages{}
			for _, m := range msgs {
				if m.Type.Level().IsWorseThanOrEqualTo(outputThreshold.Level) {
					outputMsgs = append(outputMsgs, m)
				}
			}
			output, err := formatting.Print(msgs, msgOutputFormat, true)
			if err != nil {
				return err
			}

			if len(outputMsgs) == 0 {
				fmt.Fprintf(cmd.ErrOrStderr(), color.New(color.FgGreen).Sprint("✔")+" No issues found when checking the cluster. Istio is safe to install or upgrade!\n"+
					"  To get started, check out https://istio.io/latest/docs/setup/getting-started/\n")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), output)
			}
			for _, m := range msgs {
				if m.Type.Level().IsWorseThanOrEqualTo(diag.Warning) {
					e := fmt.Sprintf(`Issues found when checking the cluster. Istio may not be safe to install or upgrade.
See %s for more information about causes and resolutions.`, url.ConfigAnalysis)
					return errors.New(e)
				}
			}
			return nil
		},
	}
	cmd.PersistentFlags().BoolVar(&skipControlPlane, "skip-controlplane", false, "skip checking the control plane")
	cmd.PersistentFlags().Var(&outputThreshold, "output-threshold",
		fmt.Sprintf("The severity level of precheck at which to display messages. Valid values: %v", diag.GetAllLevelStrings()))
	cmd.PersistentFlags().StringVarP(&msgOutputFormat, "output", "o", formatting.LogFormat,
		fmt.Sprintf("Output format: one of %v", formatting.MsgOutputFormatKeys))
	cmd.PersistentFlags().StringVarP(&fromCompatibilityVersion, "from-version", "f", "",
		"check changes since the provided version")
	opts.AttachControlPlaneFlags(cmd)
	return cmd
}

func checkFromVersion(ctx cli.Context, revision, version string) (diag.Messages, error) {
	cli, err := ctx.CLIClientWithRevision(revision)
	if err != nil {
		return nil, err
	}
	major, minors, ok := strings.Cut(version, ".")
	if !ok {
		return nil, fmt.Errorf("invalid version %v, expected format like '1.0'", version)
	}
	if major != "1" {
		return nil, fmt.Errorf("expected major version 1, got %v", version)
	}
	minor, err := strconv.Atoi(minors)
	if err != nil {
		return nil, fmt.Errorf("minor version is not a number: %v", minors)
	}

	var messages diag.Messages = make([]diag.Message, 0)
	if minor <= 20 {
		// VERIFY_CERTIFICATE_AT_CLIENT and ENABLE_AUTO_SNI
		if err := checkDestinationRuleTLS(cli, &messages); err != nil {
			return nil, err
		}
		// ENABLE_EXTERNAL_NAME_ALIAS
		if err := checkExternalNameAlias(cli, &messages); err != nil {
			return nil, err
		}
		// PERSIST_OLDEST_FIRST_HEURISTIC_FOR_VIRTUAL_SERVICE_HOST_MATCHING
		// TODO
		messages.Add(msg.NewUnknownUpgradeCompatibility(nil,
			"PERSIST_OLDEST_FIRST_HEURISTIC_FOR_VIRTUAL_SERVICE_HOST_MATCHING", "1.20",
			"consult upgrade notes for more information", "1.20"))
	}
	return messages, nil
}

func checkExternalNameAlias(cli kube.CLIClient, messages *diag.Messages) error {
	svcs, err := cli.Kube().CoreV1().Services(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, svc := range svcs.Items {
		if svc.Spec.Type != corev1.ServiceTypeExternalName {
			continue
		}
		res := ObjectToInstance(&svc)
		messages.Add(msg.NewUpdateIncompatibility(res,
			"ENABLE_EXTERNAL_NAME_ALIAS", "1.20",
			"ExternalName services now behavior differently; consult upgrade notes for more information", "1.20"))

	}
	return nil
}

func checkDestinationRuleTLS(cli kube.CLIClient, messages *diag.Messages) error {
	drs, err := cli.Istio().NetworkingV1alpha3().DestinationRules(metav1.NamespaceAll).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return err
	}
	checkVerify := func(tls *networking.ClientTLSSettings) bool {
		return tls != nil && tls.CaCertificates == "" && tls.CredentialName == "" &&
			tls.Mode != networking.ClientTLSSettings_ISTIO_MUTUAL && !tls.InsecureSkipVerify.GetValue()
	}
	checkSNI := func(tls *networking.ClientTLSSettings) bool {
		return tls != nil && tls.Sni == "" && tls.Mode != networking.ClientTLSSettings_ISTIO_MUTUAL
	}
	for _, dr := range drs.Items {
		verificationImpacted := false
		sniImpacted := false
		verificationImpacted = verificationImpacted || checkVerify(dr.Spec.GetTrafficPolicy().GetTls())
		sniImpacted = sniImpacted || checkSNI(dr.Spec.GetTrafficPolicy().GetTls())
		for _, pl := range dr.Spec.GetTrafficPolicy().GetPortLevelSettings() {
			verificationImpacted = verificationImpacted || checkVerify(pl.GetTls())
			sniImpacted = sniImpacted || checkSNI(pl.GetTls())
		}
		for _, ss := range dr.Spec.Subsets {
			verificationImpacted = verificationImpacted || checkVerify(ss.GetTrafficPolicy().GetTls())
			sniImpacted = sniImpacted || checkSNI(ss.GetTrafficPolicy().GetTls())
			for _, pl := range ss.GetTrafficPolicy().GetPortLevelSettings() {
				verificationImpacted = verificationImpacted || checkVerify(pl.GetTls())
				sniImpacted = sniImpacted || checkSNI(pl.GetTls())
			}
		}
		if verificationImpacted {
			res := ObjectToInstance(dr)
			messages.Add(msg.NewUpdateIncompatibility(res,
				"VERIFY_CERTIFICATE_AT_CLIENT", "1.20",
				"previously, TLS verification was skipped. Set `insecureSkipVerify` if this behavior is desired", "1.20"))
		}
		if sniImpacted {
			res := ObjectToInstance(dr)
			messages.Add(msg.NewUpdateIncompatibility(res,
				"ENABLE_AUTO_SNI", "1.20",
				"previously, no SNI would be set; now it will be automatically set", "1.20"))
		}
	}
	return nil
}

func ObjectToInstance(c controllers.Object) *resource.Instance {
	return &resource.Instance{
		Origin: &kube3.Origin{
			Type: kubetypes.GvkFromObject(c),
			FullName: resource.FullName{
				Namespace: resource.Namespace(c.GetNamespace()),
				Name:      resource.LocalName(c.GetName()),
			},
			ResourceVersion: resource.Version(c.GetResourceVersion()),
			Ref:             nil,
			FieldsMap:       nil,
		},
	}
}

func checkControlPlane(ctx cli.Context) (diag.Messages, error) {
	cli, err := ctx.CLIClient()
	if err != nil {
		return nil, err
	}
	msgs := diag.Messages{}

	m, err := checkServerVersion(cli)
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, m...)

	msgs = append(msgs, checkInstallPermissions(cli, ctx.IstioNamespace())...)
	gwMsg, err := checkGatewayAPIs(cli)
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, gwMsg...)

	// TODO: add more checks

	sa := local.NewSourceAnalyzer(
		analysis.Combine("upgrade precheck", &maturity.AlphaAnalyzer{}),
		resource.Namespace(ctx.Namespace()),
		resource.Namespace(ctx.IstioNamespace()),
		nil,
	)
	if err != nil {
		return nil, err
	}
	sa.AddRunningKubeSource(cli)
	cancel := make(chan struct{})
	result, err := sa.Analyze(cancel)
	if err != nil {
		return nil, err
	}
	if result.Messages != nil {
		msgs = append(msgs, result.Messages...)
	}

	return msgs, nil
}

// Checks that if the user has gateway APIs, they are the minimum version.
// It is ok to not have them, but they must be at least v1beta1 if they do.
func checkGatewayAPIs(cli kube.CLIClient) (diag.Messages, error) {
	msgs := diag.Messages{}
	res, err := cli.Ext().ApiextensionsV1().CustomResourceDefinitions().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	betaKinds := sets.New(gvk.KubernetesGateway.Kind, gvk.GatewayClass.Kind, gvk.HTTPRoute.Kind, gvk.ReferenceGrant.Kind)
	for _, r := range res.Items {
		if r.Spec.Group != gvk.KubernetesGateway.Group {
			continue
		}
		if !betaKinds.Contains(r.Spec.Names.Kind) {
			continue
		}

		versions := extractCRDVersions(&r)
		has := "none"
		if len(versions) > 0 {
			has = strings.Join(sets.SortedList(versions), ",")
		}
		if !versions.Contains(gvk.KubernetesGateway.Version) {
			origin := kube3.Origin{
				Type: gvk.CustomResourceDefinition,
				FullName: resource.FullName{
					Namespace: resource.Namespace(r.Namespace),
					Name:      resource.LocalName(r.Name),
				},
				ResourceVersion: resource.Version(r.ResourceVersion),
			}
			r := &resource.Instance{
				Origin: &origin,
			}
			msgs.Add(msg.NewUnsupportedGatewayAPIVersion(r, has, gvk.KubernetesGateway.Version))
		}
	}
	return msgs, nil
}

func extractCRDVersions(r *crd.CustomResourceDefinition) sets.String {
	res := sets.New[string]()
	for _, v := range r.Spec.Versions {
		if v.Served {
			res.Insert(v.Name)
		}
	}
	return res
}

func checkInstallPermissions(cli kube.CLIClient, istioNamespace string) diag.Messages {
	Resources := []struct {
		namespace string
		group     string
		version   string
		name      string
	}{
		{
			version: "v1",
			name:    "Namespace",
		},
		{
			namespace: istioNamespace,
			group:     "rbac.authorization.k8s.io",
			version:   "v1",
			name:      "ClusterRole",
		},
		{
			namespace: istioNamespace,
			group:     "rbac.authorization.k8s.io",
			version:   "v1",
			name:      "ClusterRoleBinding",
		},
		{
			namespace: istioNamespace,
			group:     "apiextensions.k8s.io",
			version:   "v1",
			name:      "CustomResourceDefinition",
		},
		{
			namespace: istioNamespace,
			group:     "rbac.authorization.k8s.io",
			version:   "v1",
			name:      "Role",
		},
		{
			namespace: istioNamespace,
			version:   "v1",
			name:      "ServiceAccount",
		},
		{
			namespace: istioNamespace,
			version:   "v1",
			name:      "Service",
		},
		{
			namespace: istioNamespace,
			group:     "apps",
			version:   "v1",
			name:      "Deployments",
		},
		{
			namespace: istioNamespace,
			version:   "v1",
			name:      "ConfigMap",
		},
		{
			group:   "admissionregistration.k8s.io",
			version: "v1",
			name:    "MutatingWebhookConfiguration",
		},
		{
			group:   "admissionregistration.k8s.io",
			version: "v1",
			name:    "ValidatingWebhookConfiguration",
		},
	}
	msgs := diag.Messages{}
	for _, r := range Resources {
		err := checkCanCreateResources(cli, r.namespace, r.group, r.version, r.name)
		if err != nil {
			msgs.Add(msg.NewInsufficientPermissions(&resource.Instance{Origin: clusterOrigin{}}, r.name, err.Error()))
		}
	}
	return msgs
}

func checkCanCreateResources(c kube.CLIClient, namespace, group, version, name string) error {
	s := &authorizationapi.SelfSubjectAccessReview{
		Spec: authorizationapi.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationapi.ResourceAttributes{
				Namespace: namespace,
				Verb:      "create",
				Group:     group,
				Version:   version,
				Resource:  name,
			},
		},
	}

	response, err := c.Kube().AuthorizationV1().SelfSubjectAccessReviews().Create(context.Background(), s, metav1.CreateOptions{})
	if err != nil {
		return err
	}

	if !response.Status.Allowed {
		if len(response.Status.Reason) > 0 {
			return errors.New(response.Status.Reason)
		}
		return errors.New("permission denied")
	}
	return nil
}

func checkServerVersion(cli kube.CLIClient) (diag.Messages, error) {
	v, err := cli.GetKubernetesVersion()
	if err != nil {
		return nil, fmt.Errorf("failed to get the Kubernetes version: %v", err)
	}
	compatible, err := k8sversion.CheckKubernetesVersion(v)
	if err != nil {
		return nil, err
	}
	if !compatible {
		return []diag.Message{
			msg.NewUnsupportedKubernetesVersion(&resource.Instance{Origin: clusterOrigin{}}, v.String(), fmt.Sprintf("1.%d", k8sversion.MinK8SVersion)),
		}, nil
	}
	return nil, nil
}

// clusterOrigin defines an Origin that refers to the cluster
type clusterOrigin struct{}

func (o clusterOrigin) String() string {
	return ""
}

func (o clusterOrigin) FriendlyName() string {
	return "Cluster"
}

func (o clusterOrigin) Comparator() string {
	return o.FriendlyName()
}

func (o clusterOrigin) Namespace() resource.Namespace {
	return ""
}

func (o clusterOrigin) Reference() resource.Reference {
	return nil
}

func (o clusterOrigin) FieldMap() map[string]int {
	return make(map[string]int)
}
