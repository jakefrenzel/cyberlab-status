# cyberlab-status

[![CI](https://github.com/jakefrenzel/cyberlab-status/actions/workflows/ci.yml/badge.svg)](https://github.com/jakefrenzel/cyberlab-status/actions/workflows/ci.yml)

A one-page health summary of my homelab's Kubernetes cluster, written in Go. It queries Prometheus and shows every check as a count that is **zero when healthy**: nodes not ready, pods not running, Argo CD apps out of sync, policy drops at the host firewall, Falco detections, and so on. The cluster itself is built in [cyberlab-infra](https://github.com/jakefrenzel/cyberlab-infra) and documented in [jakefrenzel-cyberlab](https://github.com/jakefrenzel/jakefrenzel-cyberlab).

The app is deliberately small. The point of this repository is the path from `git push` to a running pod: tests, linting, vulnerability scanning, a minimal container, and (next) signed images deployed by Argo CD.

## Endpoints

| Path | What |
|------|------|
| `/` | The status page (refreshes every 30 s) |
| `/api/status` | The same data as JSON |
| `/healthz` | Liveness: the process is up |
| `/metrics` | The app's own Prometheus metrics: check levels, query errors, refresh time, requests |

## Design

- **Standard library first.** The HTTP server, routing, templates, and the Prometheus API client are all standard library. The only dependency is `prometheus/client_golang` for `/metrics`
- **Checks refresh in the background every 15 seconds**, all in parallel, and page requests are served from that snapshot, so open browser tabs can't flood Prometheus. Because the refresh doesn't depend on anyone viewing the page, the app's own metrics stay current, and alerts can be built on them
- **No JavaScript.** The page refreshes with a meta tag, which allows a strict Content-Security-Policy (`default-src 'none'`; styles from this origin only). Everything rendered goes through `html/template`'s escaping
- **Fails visibly, not silently.** If Prometheus can't be reached, checks show `?` and the page says so; `/healthz` stays up, since restarting the pod wouldn't fix Prometheus

## Container

- Multi-stage build: compiled in `golang`, shipped on `distroless/static` (no shell, no package manager, no libc), as UID 65532
- A static binary built with `-trimpath`, so no local paths end up in it
- Both base images are pinned by digest, and Dependabot proposes updates weekly

## CI

Every push and pull request runs [`ci.yml`](.github/workflows/ci.yml):

| Step | Checks |
|------|--------|
| `go mod verify` | Every dependency matches its checksum in `go.sum` |
| `go vet`, `go test -race` | Correctness, including data races |
| golangci-lint | Lints including `gosec` (security), `bodyclose`, and `noctx` |
| govulncheck | Known vulnerabilities in code the program can actually reach |
| Image build + smoke test | The image starts, answers `/healthz`, and runs as nonroot |
| Trivy | Fails on HIGH or CRITICAL vulnerabilities with a fix available, in the OS layer or the Go binary |

Actions are pinned to commit SHAs and the workflow token is read-only.

## Releases

Pushing a version tag (`git tag v0.1.0 && git push origin v0.1.0`) runs [`release.yml`](.github/workflows/release.yml):

1. Build the image and scan it with Trivy. Nothing is pushed if the scan fails
2. Generate an SPDX SBOM (every OS package and Go module in the image)
3. Push to `ghcr.io/jakefrenzel/cyberlab-status` with SLSA build provenance
4. Sign the image **by digest** with cosign, and attach the SBOM as a signed attestation
5. Verify both before the job counts as done

Signing is keyless: the workflow's GitHub OIDC token gets a short-lived certificate from Sigstore that names this repository and workflow, and the signature is recorded in Sigstore's public transparency log. There's no signing key to store or leak. Only this workflow can publish or sign (`packages: write`, `id-token: write`); CI stays read-only.

Anyone can check that an image came from this repository's release workflow:

```bash
cosign verify ghcr.io/jakefrenzel/cyberlab-status:0.1.0 \
  --certificate-identity-regexp '^https://github.com/jakefrenzel/cyberlab-status/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

## Running locally

Requires Go 1.27. Against the lab's Prometheus through a port-forward:

```bash
kubectl -n monitoring port-forward svc/kube-prometheus-stack-prometheus 9090:9090
go run . -prometheus http://localhost:9090 -listen 127.0.0.1:8080
```

Then open <http://localhost:8080>. In the cluster it defaults to `http://kube-prometheus-stack-prometheus.monitoring.svc:9090`; override with `PROMETHEUS_URL` or `-prometheus`.

```bash
go test ./...
```
