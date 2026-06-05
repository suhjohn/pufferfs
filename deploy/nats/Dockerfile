FROM nats:2-alpine

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 4222 6222 8222
ENTRYPOINT ["/entrypoint.sh"]
