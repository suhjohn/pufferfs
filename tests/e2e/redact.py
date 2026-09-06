"""Remove configured credentials from retained container diagnostics."""
import os
import sys
from urllib.parse import quote, unquote, urlsplit

secrets = set()
for name, value in os.environ.items():
    if value and (name.endswith(("_KEY", "_SECRET", "_TOKEN", "_PASSWORD", "_TOKEN_ID", "_KEY_ID", "_PASSPHRASE"))
                  or name in {"DATABASE_URL", "JWT_SECRET"}):
        secrets.add(value)
    if name == "DATABASE_URL" and value:
        password = urlsplit(value).password
        if password:
            secrets.update((password, unquote(password)))
# Exceptions sometimes print credentials with URL escaping instead of the
# original environment value. Replace longer values before their components.
secrets.update(quote(value, safe="") for value in tuple(secrets))
secrets = sorted(secrets, key=len, reverse=True)
for line in sys.stdin:
    for secret in secrets:
        line = line.replace(secret, "[REDACTED]")
    sys.stdout.write(line)
