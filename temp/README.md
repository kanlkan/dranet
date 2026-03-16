# Support for Whereabouts IPAM

This is a sample implementation demonstrating how DRANET can support [Whereabouts](https://github.com/k8snetworkplumbingwg/whereabouts) IPAM.
Although Whereabouts is designed as a CNI-based IPAM plugin, DRANET can reuse its implementation as a library for IP address management.

## Preparation

To support Whereabouts IPAM, the following CRDs need to be created:

```
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/whereabouts/v0.9.3/doc/crds/whereabouts.cni.cncf.io_ippools.yaml
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/whereabouts/v0.9.3/doc/crds/whereabouts.cni.cncf.io_overlappingrangeipreservations.yaml
kubectl apply -f https://raw.githubusercontent.com/k8snetworkplumbingwg/whereabouts/v0.9.3/doc/crds/whereabouts.cni.cncf.io_nodeslicepools.yaml

```

## Examples

Cluster administrators create a DeviceClass that includes an IP range available for users.
Example: [deviceclass-hidden-ipam.yaml](./deviceclass-hidden-ipam.yaml)

Apply the following manifests to run the demonstration:

```
kubectl apply -f deviceclass-hidden-ipam.yaml
kubectl apply -f deployment-tenant-a.yaml
kubectl apply -f deployment-tenant-b.yaml
kubectl apply -f deployment-tenant-c.yaml
```

One of the pod for tenant-c deployment will fail to start because no IP address remain.

(This DeviceClass provides only 5 IP addresses.)
