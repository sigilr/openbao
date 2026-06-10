IN this document Vault Server is same as Openbao server. Vault Agent is same as Openbao agent.

We have implmented a hub-spoke model for Openbao. Lear about it from https://github.com/kubevault/openbao/blob/remote-db-plugin/plugins/database/remote-db-plugin/README.md . Read the https://github.com/kubevault/openbao/blob/remote-db-plugin/plugins/database/remote-db-plugin/DESIGN.md to understand it better. The pr is https://github.com/kubevault/openbao/pull/1


Now, we have to implement a Kubernetes Operator that deploys OpenBao server and agents across a hub and many spoke clusters.

- The Kubernetes operator apis are defined here: /Users/tamal/go/src/kubevault.dev/apimachinery
The ocm-add branch has a VaultServer and VaultAgent CRD.
pr: https://github.com/kubevault/apimachinery/pull/136

- The Kubernetes operator implementation is here: /Users/tamal/go/src/kubevault.dev/operator in the ocm-add branch
pr: https://github.com/kubevault/operator/pull/188

The hub-spoke model is defined by https://open-cluster-management.io/ aka ocm

Specifically in the VaultServer crd add a spec.agentPlacementRef field that points to a OCM Placement object https://open-cluster-management.io/docs/concepts/content-placement/placement/

Then implement a new section in the KubeVault Vaultserver operator that will deploy VaultAgent cr to the managed cluster using ManifestWork.

Use Placement and PlacementDecision to detect the managed clusters where the VaultAgent will be deployed.

Then use a seperate ManifestWork per selected Managed Cluster to deploy VaultAgent.
In the ManifestWork, there should be VaultAgent, AppBinding for the Vaultserver, the auth method should use a service account from the hub cluster that lives in the managed cluster namespace in the hub cluster. There should be necessary VaultRole and VaultRoleBinding for this service account to have necessry permissions on Vault Server.
Also, understand the init and join process for the VaultServer / Opnebao to join as such. The VaultServer should be exposed via a LoadBalancer service so that spoke clusters can reach them. That LB address should be used in the AppBinding for the spoke cluster.

Learn from:
- https://open-cluster-management.io/docs/concepts/work-distribution/manifestwork/
- https://github.com/open-cluster-management-io/ocm/tree/main/pkg/work/hub/controllers/manifestworkreplicasetcontroller
- https://github.com/open-cluster-management-io/api/blob/main/cluster/v1beta1/types_placementdecision.go
- https://github.com/open-cluster-management-io/api/blob/main/cluster/v1beta1/types_placement.go


Create a Design doc for implementing this feature. Research well first to do so
