# CRI validation with critest

To validate urunc's CRI surface against something upstream, we run
[critest](https://github.com/kubernetes-sigs/cri-tools) from kubernetes-sigs/cri-tools
against containerd with `--runtime-handler=urunc`. The workflow lives in
`.github/workflows/critest.yml`.

critest drives generic Linux images. Under the urunc handler these hit the
runc-delegation path, not the unikernel path. This is exactly the plumbing
Kubernetes exercises for the sandbox and sidecar containers -- unikernel
behavior is covered by the e2e suite in `tests/e2e`.

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

Baseline (2026-07-23, critest v1.36.0, containerd 2.3.3): 41/50 Conformance
specs pass; all 9 failures trace to exec not being supported by the shim for
runc-delegated containers.
