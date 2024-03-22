# httpnc
HTTP client and server in a single binary for transferring stdin on one host to stdout on another like nc/socat

## Installation
```
go install github.com/ihanick/httpnc/cmd/httpnc@latest
```

## Usage

Generate ssl keys for the server
```
openssl req -x509 -nodes -newkey rsa:2048 -keyout key.pem -out cert.pem -days 365
```
Start the server on the destination:
```
httpnc -l :8443 -token token123 | tar xz
```
Initiate transer on the source side:
```
tar cz mydir | httpnc -connect https://localhost:8443/upload -token token123
```

### SST for PXC

```diff
--- wsrep_sst_xtrabackup-v2.orig        2024-03-22 14:48:12.886735616 +0300
+++ wsrep_sst_xtrabackup-v2     2024-03-22 14:45:01.467348976 +0300
@@ -504,6 +504,16 @@ get_transfer()
             # netcat doesn't understand [] around IPv6 address
             tcmd="nc ${tcmd_extra} ${REMOTEIP//[\[\]]/} ${TSST_PORT}"
         fi
+    elif [[ $tfmt == 'httpnc' ]]; then
+        wsrep_log_debug "Using httpnc as streamer"
+        if [[ "$WSREP_SST_OPT_ROLE"  == "joiner" ]]; then
+                ssl_cert=$(get_absolute_path "$ssl_cert")
+                ssl_key=$(get_absolute_path "$ssl_key")
+
+                tcmd="httpnc -l :${TSST_PORT} -token token123 -key ${ssl_key} -cert ${ssl_cert}"
+        else
+                tcmd="httpnc -connect https://${REMOTEIP}:${TSST_PORT}/upload  -token token123"
+        fi
     else
         tfmt='socat'
         wsrep_log_debug "Using socat as streamer"
@@ -1095,7 +1105,7 @@ wait_for_listen()

         for i in {1..300}
         do
-            ss -p state listening "( sport = :${port} )" | grep -qE 'socat|nc' && break
+            ss -p state listening "( sport = :${port} )" | grep -qE 'socat|nc|httpnc' && break
             sleep 0.2
         done

@@ -1151,7 +1161,7 @@ wait_for_listen()
         wsrep_log_debug "$LINENO: Entering loop body : $i"

         # List only socat/nc processes started by this user to avoid triggering SELinux
-        for pid in $(ps -u $user_id -o pid,comm | grep -E 'socat|nc' | awk '{ printf $1 " " }')
+        for pid in $(ps -u $user_id -o pid,comm | grep -E 'socat|nc|httpnc' | awk '{ printf $1 " " }')
         do
             if [[ -z $pid || $pid = " " ]]; then
                 continue
```

The my.cnf configuration requires:
```
[sst]
transferfmt=httpnc
```
