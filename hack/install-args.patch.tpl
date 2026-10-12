# A5: JSON6902 patch that APPENDS the five ghcr image args to the manager
# container (the release's public images, all tagged with IMG's tag).
#
# One `op: add` per flag, to /spec/template/spec/containers/0/args/- (the
# array-appending form). JSON6902 does NOT replace the container's existing
# args, so the base args (--metrics-bind-address, --leader-elect,
# --health-probe-bind-address) are preserved (P1-1: a strategic-merge patch on
# args replaces the whole list, which would drop --health-probe-bind-address
# and crash-loop the released operator). The egress proxy flag (--egress-proxy
# -image) is appended too so a released install never pulls the
# coxswain-egress-proxy:standin stand-in (P1-2).
# __TAG__ is rendered to IMG's tag by hack/build-installer.sh (sed).
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: --runner-image=ghcr.io/papawattu/coxswain-runner:__TAG__
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: --proxy-image=ghcr.io/papawattu/coxswain-model-proxy:__TAG__
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: --tool-proxy-image=ghcr.io/papawattu/coxswain-tool-proxy:__TAG__
- op: add
  path: /spec/template/spec/containers/0/args/-
  value: --egress-proxy-image=ghcr.io/papawattu/coxswain-egress-proxy:__TAG__
