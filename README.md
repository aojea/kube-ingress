# Kube-ingress: Ingress API Reference Implementation

This repository provides a lightweight, reference implementation of the official Kubernetes Ingress API (`networking.k8s.io/v1`).

## Core Philosophy and Scope

This controller's scope is intentionally limited to providing a secure, stable, and compliant implementation of the official API only.

* **Strictly Adheres to the Spec:** We only implement the fields and behaviors defined in the `networking.k8s.io/v1` Ingress and IngressClass resources.

* **Annotations are Out of Scope:** This project will not extend functionality using custom annotations (e.g., xxxxx.ingress.kubernetes.io/...). This ensures the controller remains simple, secure, and focused purely on the official API contract.

## Looking for More Features

If your use case requires functionality not covered by the standard Ingress API (such as complex routing, traffic splitting, or custom configurations often handled by annotations), this project is not the right fit.

We strongly recommend you explore one of the following:

* **Feature-rich Ingress Controllers:** There are already OSS third-party controllers that provide extensive features via custom annotations and CRDs.

* **The Gateway API:** For modern, expressive, and role-oriented networking, please look to the official [Kubernetes Gateway API](https://gateway-api.sigs.k8s.io/). It is the designated successor to the Ingress API for advanced use cases.

## Install

Just apply the provided manifest, which includes the Deployment, Service, and necessary RBAC rules:

```sh
kubectl apply -f install.yaml
```

You can scale the number of ingress controller replicas up or down at any time using kubectl scale. For example, to scale to 3 replicas:

```sh
kubectl scale deployment kube-ingress -n kube-system --replicas=3
```

## Community, discussion, contribution, and support

Learn how to engage with the Kubernetes community on the [community page](http://kubernetes.io/community/).

You can reach the maintainers of this project at:

- [Slack](https://slack.k8s.io/)
- [Mailing List](https://groups.google.com/a/kubernetes.io/g/dev)

### Code of conduct

Participation in the Kubernetes community is governed by the [Kubernetes Code of Conduct](code-of-conduct.md).

[owners]: https://git.k8s.io/community/contributors/guide/owners.md
[Creative Commons 4.0]: https://git.k8s.io/website/LICENSE
