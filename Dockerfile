FROM python:3.14-slim

ENV SCHEDULE="0 0 * * *" TZ=Asia/Shanghai PYTHONUNBUFFERED=1

WORKDIR /usr/src/db-auto-backup
RUN mkdir -p /var/backups

COPY requirements.txt .
RUN apt-get update -qq \
 && apt-get install --no-install-recommends -y git tzdata \
 && pip install --no-cache-dir -r requirements.txt \
 && apt-get purge -y git \
 && apt-get autoremove -y \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* /root/.cache

COPY db-auto-backup.py .

CMD ["python3", "./db-auto-backup.py"]
