FROM python:3.12-slim

WORKDIR /app

COPY agent_of_chaos.py benchmark_profiles.json ./

RUN mkdir -p /var/log/agent-of-chaos \
    && useradd --create-home --uid 10001 --shell /usr/sbin/nologin appuser \
    && chown -R appuser:appuser /var/log/agent-of-chaos /app

ENV PYTHONUNBUFFERED=1 \
    AOCH_LOG_DIR=/var/log/agent-of-chaos \
    AOCH_MODE=steady \
    AOCH_SERVICES=5

USER appuser

VOLUME ["/var/log/agent-of-chaos"]

ENTRYPOINT ["python3", "/app/agent_of_chaos.py"]
