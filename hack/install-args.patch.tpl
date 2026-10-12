apiVersion: apps/v1
kind: Deployment
metadata:
  name: controller-manager
spec:
  template:
    spec:
      containers:
        - name: manager
          args:
            - "--runner-image=ghcr.io/papawattu/coxswain-runner:__TAG__"
            - "--proxy-image=ghcr.io/papawattu/coxswain-model-proxy:__TAG__"
            - "--tool-proxy-image=ghcr.io/papawattu/coxswain-tool-proxy:__TAG__"
