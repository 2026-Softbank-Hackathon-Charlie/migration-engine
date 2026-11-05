"""Build a token kubeconfig for the runner. stdin: {"kc": <kubectl config view --raw -o json>, "secret": <token Secret json>}.
argv: server out_path. Writes the file (0600) and prints nothing secret."""
import base64, json, os, sys
d = json.load(sys.stdin)
server, out = sys.argv[1:]
ca = d["kc"]["clusters"][0]["cluster"]["certificate-authority-data"]
tok = base64.b64decode(d["secret"]["data"]["token"]).decode()
cfg = {"apiVersion": "v1", "kind": "Config", "current-context": "gke",
       "clusters": [{"name": "gke", "cluster": {"server": server, "certificate-authority-data": ca}}],
       "users": [{"name": "moimroom-runner", "user": {"token": tok}}],
       "contexts": [{"name": "gke", "context": {"cluster": "gke", "user": "moimroom-runner", "namespace": "charlie-apps"}}]}
fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
with os.fdopen(fd, "w") as f:
    json.dump(cfg, f)
print("wrote", out)
