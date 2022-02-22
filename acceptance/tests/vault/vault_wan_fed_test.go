package vault

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/consul-k8s/acceptance/framework/config"
	"github.com/hashicorp/consul-k8s/acceptance/framework/consul"
	"github.com/hashicorp/consul-k8s/acceptance/framework/environment"
	"github.com/hashicorp/consul-k8s/acceptance/framework/helpers"
	"github.com/hashicorp/consul-k8s/acceptance/framework/k8s"
	"github.com/hashicorp/consul-k8s/acceptance/framework/logger"
	"github.com/hashicorp/consul-k8s/acceptance/framework/vault"
	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Test that WAN federation via Mesh gateways works with Vault
// as the secrets backend, testing all possible credentials that can be used for WAN federation.
// This test deploys a Vault cluster with a server in the primary k8s cluster and exposes it to the
// secondary cluster via a Kubernetes service. We then only need to deploy Vault agent injector
// in the secondary that will treat the Vault server in the primary as an external server.
func TestVault_WANFederationViaGateways(t *testing.T) {
	cfg := suite.Config()
	if !cfg.EnableMultiCluster {
		t.Skipf("skipping this test because -enable-multi-cluster is not set")
	}
	primaryCtx := suite.Environment().DefaultContext(t)
	secondaryCtx := suite.Environment().Context(t, environment.SecondaryContextName)

	ns := primaryCtx.KubectlOptions(t).Namespace

	vaultReleaseName := helpers.RandomName()
	consulReleaseName := helpers.RandomName()

	// In the primary cluster, we will expose Vault server as a Load balancer
	// or a NodePort service so that the secondary can connect to it.
	primaryVaultHelmValues := map[string]string{
		"server.service.type": "LoadBalancer",
	}
	if cfg.UseKind {
		primaryVaultHelmValues["server.service.type"] = "NodePort"
		primaryVaultHelmValues["server.service.nodePort"] = "31000"
	}

	primaryVaultCluster := vault.NewVaultCluster(t, primaryCtx, cfg, vaultReleaseName, primaryVaultHelmValues)
	primaryVaultCluster.Create(t, primaryCtx)

	externalVaultAddress := vaultAddress(t, cfg, primaryCtx, vaultReleaseName)

	// In the secondary cluster, we will only deploy the agent injector and provide
	// it with the primary's Vault address. We also want to configure the injector with
	// a different k8s auth method path since the secondary cluster will need its own auth method.
	secondaryVaultHelmValues := map[string]string{
		"server.enabled":             "false",
		"injector.externalVaultAddr": externalVaultAddress,
		"injector.authPath":          "auth/kubernetes-dc2",
	}

	secondaryVaultCluster := vault.NewVaultCluster(t, secondaryCtx, cfg, vaultReleaseName, secondaryVaultHelmValues)
	secondaryVaultCluster.Create(t, secondaryCtx)

	vaultClient := primaryVaultCluster.VaultClient(t)

	configureGossipVaultSecret(t, vaultClient)

	if cfg.EnableEnterprise {
		configureEnterpriseLicenseVaultSecret(t, vaultClient, cfg)
	}

	configureKubernetesAuthRoles(t, vaultClient, consulReleaseName, ns, "kubernetes", "dc1", cfg)

	// Configure Vault Kubernetes auth method for the secondary datacenter.
	{
		// Create auth method service account and ClusterRoleBinding. The Vault server
		// in the primary cluster will use this service account token to talk to the secondary
		// Kubernetes cluster.
		// This ClusterRoleBinding is adapted from the Vault server's role:
		// https://github.com/hashicorp/vault-helm/blob/b0528fce49c529f2c37953ea3a14f30ed651e0d6/templates/server-clusterrolebinding.yaml

		// Use a single name for all RBAC objects.
		authMethodRBACName := fmt.Sprintf("%s-vault-auth-method", vaultReleaseName)
		_, err := secondaryCtx.KubernetesClient(t).RbacV1().ClusterRoleBindings().Create(context.Background(), &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: authMethodRBACName,
			},
			Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: authMethodRBACName, Namespace: ns}},
			RoleRef:  rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Name: "system:auth-delegator", Kind: "ClusterRole"},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		// Create service account for the auth method in the secondary cluster.
		_, err = secondaryCtx.KubernetesClient(t).CoreV1().ServiceAccounts(ns).Create(context.Background(), &corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name: authMethodRBACName,
			},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		t.Cleanup(func() {
			secondaryCtx.KubernetesClient(t).RbacV1().ClusterRoleBindings().Delete(context.Background(), authMethodRBACName, metav1.DeleteOptions{})
			secondaryCtx.KubernetesClient(t).CoreV1().ServiceAccounts(ns).Delete(context.Background(), authMethodRBACName, metav1.DeleteOptions{})
		})

		// Figure out the host for the Kubernetes API. This needs to be reachable from the Vault server
		// in the primary cluster.
		k8sAuthMethodHost := k8s.KubernetesAPIServerHost(t, cfg, secondaryCtx)

		// Now, configure the auth method in Vault.
		secondaryVaultCluster.ConfigureAuthMethod(t, vaultClient, "kubernetes-dc2", k8sAuthMethodHost, authMethodRBACName, ns)
	}

	configureKubernetesAuthRoles(t, vaultClient, consulReleaseName, ns, "kubernetes-dc2", "dc2", cfg)

	// Generate a CA and create PKI roles for the primary and secondary Consul servers.
	configurePKICA(t, vaultClient)
	primaryCertPath := configurePKICertificates(t, vaultClient, consulReleaseName, ns, "dc1")
	secondaryCertPath := configurePKICertificates(t, vaultClient, consulReleaseName, ns, "dc2")

	replicationToken := configureReplicationTokenVaultSecret(t, vaultClient, consulReleaseName, ns, "kubernetes", "kubernetes-dc2")

	// Create the Vault Policy for the Connect CA in both datacenters.
	createConnectCAPolicy(t, vaultClient, "dc1")
	createConnectCAPolicy(t, vaultClient, "dc2")

	// Move Vault CA secret from primary to secondary so that we can mount it to pods in the
	// secondary cluster.
	vaultCASecretName := vault.CASecretName(vaultReleaseName)
	logger.Logf(t, "retrieving Vault CA secret %s from the primary cluster and applying to the secondary", vaultCASecretName)
	vaultCASecret, err := primaryCtx.KubernetesClient(t).CoreV1().Secrets(primaryCtx.KubectlOptions(t).Namespace).Get(context.Background(), vaultCASecretName, metav1.GetOptions{})
	vaultCASecret.ResourceVersion = ""
	require.NoError(t, err)
	_, err = secondaryCtx.KubernetesClient(t).CoreV1().Secrets(secondaryCtx.KubectlOptions(t).Namespace).Create(context.Background(), vaultCASecret, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		secondaryCtx.KubernetesClient(t).CoreV1().Secrets(ns).Delete(context.Background(), vaultCASecretName, metav1.DeleteOptions{})
	})

	primaryConsulHelmValues := map[string]string{
		"global.datacenter": "dc1",

		"global.federation.enabled": "true",

		// TLS config.
		"global.tls.enabled":           "true",
		"global.tls.enableAutoEncrypt": "true",
		"global.tls.caCert.secretName": "pki/cert/ca",
		"server.serverCert.secretName": primaryCertPath,

		// Gossip config.
		"global.gossipEncryption.secretName": "consul/data/secret/gossip",
		"global.gossipEncryption.secretKey":  "gossip",

		// ACL config.
		"global.acls.manageSystemACLs":            "true",
		"global.acls.createReplicationToken":      "true",
		"global.acls.replicationToken.secretName": "consul/data/secret/replication",
		"global.acls.replicationToken.secretKey":  "replication",

		// Mesh config.
		"connectInject.enabled": "true",
		"controller.enabled":    "true",
		"meshGateway.enabled":   "true",
		"meshGateway.replicas":  "1",

		// Server config.
		"server.extraVolumes[0].type": "secret",
		"server.extraVolumes[0].name": vaultCASecretName,
		"server.extraVolumes[0].load": "false",

		// Vault config.
		"global.secretsBackend.vault.enabled":                       "true",
		"global.secretsBackend.vault.consulServerRole":              "consul-server",
		"global.secretsBackend.vault.consulClientRole":              "consul-client",
		"global.secretsBackend.vault.consulCARole":                  "consul-ca",
		"global.secretsBackend.vault.manageSystemACLsRole":          "server-acl-init",
		"global.secretsBackend.vault.ca.secretName":                 vaultCASecretName,
		"global.secretsBackend.vault.ca.secretKey":                  "tls.crt",
		"global.secretsBackend.vault.connectCA.address":             primaryVaultCluster.Address(),
		"global.secretsBackend.vault.connectCA.rootPKIPath":         "connect_root",
		"global.secretsBackend.vault.connectCA.intermediatePKIPath": "dc1/connect_inter",
	}

	if cfg.EnableEnterprise {
		primaryConsulHelmValues["global.enterpriseLicense.secretName"] = "consul/data/secret/enterpriselicense"
		primaryConsulHelmValues["global.enterpriseLicense.secretKey"] = "enterpriselicense"
	}

	if cfg.UseKind {
		primaryConsulHelmValues["meshGateway.service.type"] = "NodePort"
		primaryConsulHelmValues["meshGateway.service.nodePort"] = "30000"
	}

	primaryConsulCluster := consul.NewHelmCluster(t, primaryConsulHelmValues, primaryCtx, cfg, consulReleaseName)
	primaryConsulCluster.Create(t)

	// Get the address of the mesh gateway.
	primaryMeshGWAddress := meshGatewayAddress(t, cfg, primaryCtx, consulReleaseName)
	secondaryConsulHelmValues := map[string]string{
		"global.datacenter": "dc2",

		"global.federation.enabled":            "true",
		"global.federation.primaryDatacenter":  "dc1",
		"global.federation.primaryGateways[0]": primaryMeshGWAddress,

		// TLS config.
		"global.tls.enabled":           "true",
		"global.tls.enableAutoEncrypt": "true",
		"global.tls.caCert.secretName": "pki/cert/ca",
		"server.serverCert.secretName": secondaryCertPath,

		// Gossip config.
		"global.gossipEncryption.secretName": "consul/data/secret/gossip",
		"global.gossipEncryption.secretKey":  "gossip",

		// ACL config.
		"global.acls.manageSystemACLs":            "true",
		"global.acls.replicationToken.secretName": "consul/data/secret/replication",
		"global.acls.replicationToken.secretKey":  "replication",

		// Mesh config.
		"connectInject.enabled": "true",
		"meshGateway.enabled":   "true",
		"meshGateway.replicas":  "1",

		// Server config.
		"server.extraVolumes[0].type": "secret",
		"server.extraVolumes[0].name": vaultCASecretName,
		"server.extraVolumes[0].load": "false",

		// Vault config.
		"global.secretsBackend.vault.enabled":                       "true",
		"global.secretsBackend.vault.consulServerRole":              "consul-server",
		"global.secretsBackend.vault.consulClientRole":              "consul-client",
		"global.secretsBackend.vault.consulCARole":                  "consul-ca",
		"global.secretsBackend.vault.manageSystemACLsRole":          "server-acl-init",
		"global.secretsBackend.vault.ca.secretName":                 vaultCASecretName,
		"global.secretsBackend.vault.ca.secretKey":                  "tls.crt",
		"global.secretsBackend.vault.agentAnnotations":              fmt.Sprintf("vault.hashicorp.com/tls-server-name: %s-vault", vaultReleaseName),
		"global.secretsBackend.vault.connectCA.address":             externalVaultAddress,
		"global.secretsBackend.vault.connectCA.authMethodPath":      "kubernetes-dc2",
		"global.secretsBackend.vault.connectCA.rootPKIPath":         "connect_root",
		"global.secretsBackend.vault.connectCA.intermediatePKIPath": "dc2/connect_inter",
		"global.secretsBackend.vault.connectCA.additionalConfig":    fmt.Sprintf(`"{"connect": [{"ca_config": [{"tls_server_name": "%s-vault"}]}]}"`, vaultReleaseName),
	}

	if cfg.EnableEnterprise {
		secondaryConsulHelmValues["global.enterpriseLicense.secretName"] = "consul/data/secret/enterpriselicense"
		secondaryConsulHelmValues["global.enterpriseLicense.secretKey"] = "enterpriselicense"
	}

	if cfg.UseKind {
		secondaryConsulHelmValues["meshGateway.service.type"] = "NodePort"
		secondaryConsulHelmValues["meshGateway.service.nodePort"] = "30000"
	}

	// Install the secondary consul cluster in the secondary kubernetes context.
	secondaryConsulCluster := consul.NewHelmCluster(t, secondaryConsulHelmValues, secondaryCtx, cfg, consulReleaseName)
	secondaryConsulCluster.Create(t)

	// Verify federation between servers.
	logger.Log(t, "verifying federation was successful")
	primaryClient := primaryConsulCluster.SetupConsulClient(t, true)
	secondaryConsulCluster.ACLToken = replicationToken
	secondaryClient := secondaryConsulCluster.SetupConsulClient(t, true)
	helpers.VerifyFederation(t, primaryClient, secondaryClient, consulReleaseName, true)

	// Create a ProxyDefaults resource to configure services to use the mesh
	// gateways.
	logger.Log(t, "creating proxy-defaults config")
	kustomizeDir := "../fixtures/bases/mesh-gateway"
	k8s.KubectlApplyK(t, primaryCtx.KubectlOptions(t), kustomizeDir)
	helpers.Cleanup(t, cfg.NoCleanupOnFailure, func() {
		k8s.KubectlDeleteK(t, primaryCtx.KubectlOptions(t), kustomizeDir)
	})

	// Check that we can connect services over the mesh gateways.
	logger.Log(t, "creating static-server in dc2")
	k8s.DeployKustomize(t, secondaryCtx.KubectlOptions(t), cfg.NoCleanupOnFailure, cfg.DebugDirectory, "../fixtures/cases/static-server-inject")

	logger.Log(t, "creating static-client in dc1")
	k8s.DeployKustomize(t, primaryCtx.KubectlOptions(t), cfg.NoCleanupOnFailure, cfg.DebugDirectory, "../fixtures/cases/static-client-multi-dc")

	logger.Log(t, "creating intention")
	_, _, err = primaryClient.ConfigEntries().Set(&api.ServiceIntentionsConfigEntry{
		Kind: api.ServiceIntentions,
		Name: "static-server",
		Sources: []*api.SourceIntention{
			{
				Name:   "static-client",
				Action: api.IntentionActionAllow,
			},
		},
	}, nil)
	require.NoError(t, err)

	logger.Log(t, "checking that connection is successful")
	k8s.CheckStaticServerConnectionSuccessful(t, primaryCtx.KubectlOptions(t), staticClientName, "http://localhost:1234")
}

// vaultAddress returns Vault's server URL depending on test configuration.
func vaultAddress(t *testing.T, cfg *config.TestConfig, ctx environment.TestContext, vaultReleaseName string) string {
	vaultHost := k8s.ServiceHost(t, cfg, ctx, fmt.Sprintf("%s-vault", vaultReleaseName))
	if cfg.UseKind {
		return fmt.Sprintf("https://%s:31000", vaultHost)
	}
	return fmt.Sprintf("https://%s:8200", vaultHost)
}

// meshGatewayAddress returns a full address of the mesh gateway depending on configuration.
func meshGatewayAddress(t *testing.T, cfg *config.TestConfig, ctx environment.TestContext, consulReleaseName string) string {
	primaryMeshGWHost := k8s.ServiceHost(t, cfg, ctx, fmt.Sprintf("%s-consul-mesh-gateway", consulReleaseName))
	if cfg.UseKind {
		return fmt.Sprintf("%s:%d", primaryMeshGWHost, 30000)
	} else {
		return fmt.Sprintf("%s:%d", primaryMeshGWHost, 443)
	}
}