# CRI validation with critest

To validate urunc's CRI surface against something upstream, we run
[critest](https://github.com/kubernetes-sigs/cri-tools) from kubernetes-sigs/cri-tools
against containerd with `--runtime-handler=urunc`. The workflow lives in
`.github/workflows/critest.yml`.

The workflow runs critest twice:

1. **Full suite, generic images.** These hit the runc-delegation path, not
   the VM path. This is exactly the plumbing Kubernetes exercises for the
   sandbox and sidecar containers of every urunc pod.
2. **VM path.** Via `--test-images-file` we swap critest's images for urunc
   guest images (`busybox-agent-qemu-linux-raw` and
   `nginx-qemu-linux-raw`), so every spec boots a real guest VM. exec
   reaches those guests through the urunit-agent transport. This needs the
   devmapper snapshotter (block rootfs), hence the separate
   `urunc-devmapper` handler in the containerd config, and its skips live in
   `skips-vmpath.txt`.

Unikernel behavior beyond that is covered by the e2e suite in `tests/e2e`.

There is no official CRI conformance badge. The ecosystem norm is
"passes critest vX.Y with documented skips" -- for reference, Kata runs an
allowlist of containerd's cri-integration tests and firecracker-containerd
diffs critest failures against a golden file.

## Skips

`skips.txt` holds one ginkgo skip regex per line, each with a one-line reason.
The workflow joins them with `|` and passes the result to `--ginkgo.skip`.
When a feature lands (eg. exec support in the shim), remove the corresponding
lines and the specs start gating.

## Running locally

```console
sudo critest \
  --runtime-endpoint unix:///run/containerd/containerd.sock \
  --runtime-handler urunc \
  --ginkgo.focus='\[Conformance\]' \
  --ginkgo.skip="$(grep -vE '^\s*(#|$)' skips.txt | paste -sd'|' -)"
```

Baselines (critest v1.36.0, containerd 2.3.3). Generic images, 2026-07-23:
the full validation suite passes -- 107/107 specs, 0 failures, no skip list.
The remaining 15 specs auto-skip as host/feature-gated (SELinux needs an
enforcing host, NRI needs the socket enabled, image-volume and
user-namespaces are feature-gated). Real guest VMs, 2026-07-25: 45 of 49
[Conformance] specs pass, with the five entries in `skips-vmpath.txt`
accounting for the rest.

The VM-path run allows ginkgo flake attempts, which the generic run does not.
Exec reaches a guest through a detached proxy that bridges the shim's exec IO
to the guest agent, and that handover is not yet airtight: a command's output
occasionally goes missing, more often on slower machines. Driving urunc
directly is reliable, so the fault is on the containerd side of the proxy, in
how the exec IO is torn down when the proxy exits. The retries keep the specs
running -- and keep reporting -- until that is fixed; they are not a licence
to leave it broken.

Before the exec delegation patch (feat: Add exec command with runc
delegation) the score was 41/50 on the [Conformance] subset, with all 9
failures tracing to exec not being supported for runc-delegated containers.
Note that the AppArmor specs need apparmor_parser installed before containerd
starts, as containerd probes AppArmor support once at startup.
