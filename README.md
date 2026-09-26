# Coxswain

A Kubernetes operator that runs long-lived **plan → implement → verify** loops for coding agents. Each loop runs in an isolated [agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) workspace, stops on success, budget or stall, and hands back a pull request.

Status: pre-alpha, planning. See [docs/PLAN.md](docs/PLAN.md) for the build plan.
