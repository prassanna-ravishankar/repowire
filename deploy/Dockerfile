FROM python:3.12-slim

WORKDIR /app

RUN pip install --no-cache-dir uv

COPY pyproject.toml .
COPY repowire/ repowire/

RUN uv pip install --system ".[relay]"

EXPOSE 8000

CMD ["python", "-m", "uvicorn", "repowire.relay.server:socket_app", "--host", "0.0.0.0", "--port", "8000"]
