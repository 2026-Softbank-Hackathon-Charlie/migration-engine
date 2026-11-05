"""stdin: Secret json. stdout: shell `export K='V'` lines for eval (never echoed). argv: keys to skip."""
import base64, json, shlex, sys
d = json.load(sys.stdin).get("data", {})
for k, v in d.items():
    if k in sys.argv[1:]:
        continue
    print(f"export {k}={shlex.quote(base64.b64decode(v).decode())}")
