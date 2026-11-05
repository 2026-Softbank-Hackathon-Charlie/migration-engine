"""Copy a Kubernetes Secret between clusters without printing values.

stdin: `kubectl get secret -o json`; stdout: a Secret manifest for `kubectl apply -f -`.
usage: secretx.py NAMESPACE NEW_NAME TYPE KEYMAP...      KEYMAP = dst=src  or  key
       secretx.py NAMESPACE NEW_NAME dburl HOST:PORT      (uri from username/password/dbname)
"""
import base64, json, sys, urllib.parse

src = json.load(sys.stdin)
ns, name, kind, *rest = sys.argv[1:]
data = src.get("data", {})
dec = lambda k: base64.b64decode(data[k]).decode()
if kind == "dburl":
    u, p, d = (urllib.parse.quote(dec(k), safe="") for k in ("username", "password", "dbname"))
    out = {"uri": base64.b64encode(f"postgresql://{u}:{p}@{rest[0]}/{d}".encode()).decode()}
    kind = "Opaque"
else:
    out = {}
    for m in rest:
        dst, _, s = m.partition("=")
        out[dst] = data[s or dst]
json.dump({"apiVersion": "v1", "kind": "Secret", "type": kind,
           "metadata": {"name": name, "namespace": ns,
                        "labels": {"hopcharlie.dev/migration": "moimroom-eks-to-gke"}},
           "data": out}, sys.stdout)
