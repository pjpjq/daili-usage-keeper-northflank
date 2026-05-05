FROM ghcr.io/willxup/cpa-usage-keeper:latest

ENV APP_PORT=8080 \
    CPA_BASE_URL=https://pjpjq-daili.hf.space \
    REDIS_QUEUE_ADDR=127.0.0.1:9 \
    AUTH_ENABLED=true \
    TZ=Asia/Shanghai \
    WORK_DIR=/data \
    LOG_FILE_ENABLED=true \
    BACKUP_ENABLED=true

COPY entrypoint.space.sh /usr/local/bin/entrypoint.space.sh
RUN chmod +x /usr/local/bin/entrypoint.space.sh

ENTRYPOINT ["/usr/local/bin/entrypoint.space.sh"]
CMD ["/app/cpa-usage-keeper"]
