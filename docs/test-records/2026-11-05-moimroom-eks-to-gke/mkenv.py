"""Build the runner env Secret. stdin: {"db":..,"s3":..,"app":..,"gcs":..,"ssm":<aws ssm get-parameter json>}
argv: gateway_admin_hostport. stdout: Secret manifest. Prints no values elsewhere."""
import base64, json, sys
d = json.load(sys.stdin)
dec = lambda s, k: base64.b64decode(d[s]["data"][k]).decode()
env = {
    "MOIMROOM_PGPASSWORD": dec("db", "password"),
    "GATEWAY_ADMIN": sys.argv[1],
    "GATEWAY_TOKEN": d["ssm"]["Parameter"]["Value"],
    "HOST_CODE": dec("app", "HOST_CODE"),
    "RCLONE_CONFIG_SRC_TYPE": "s3", "RCLONE_CONFIG_SRC_PROVIDER": "AWS",
    "RCLONE_CONFIG_SRC_REGION": "ap-northeast-2",
    "RCLONE_CONFIG_SRC_ACCESS_KEY_ID": dec("s3", "accessKey"),
    "RCLONE_CONFIG_SRC_SECRET_ACCESS_KEY": dec("s3", "secretKey"),
    "RCLONE_CONFIG_DST_TYPE": "s3", "RCLONE_CONFIG_DST_PROVIDER": "GCS",
    "RCLONE_CONFIG_DST_ENDPOINT": "https://storage.googleapis.com",
    "RCLONE_CONFIG_DST_ACCESS_KEY_ID": dec("gcs", "accessKey"),
    "RCLONE_CONFIG_DST_SECRET_ACCESS_KEY": dec("gcs", "secretKey"),
}
json.dump({"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
           "metadata": {"name": "moimroom-runner-env", "namespace": "hop-migrate"},
           "stringData": env}, sys.stdout)
