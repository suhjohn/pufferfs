# Agent Notes

- Local integration runs can use the repository `.env`; load it with `set -a; source .env; set +a` before running commands that need Modal, Turbopuffer, AWS, or other service credentials.
- Do not print `.env` values or include secrets in logs. If checking available variables, redact values.
