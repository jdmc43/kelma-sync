FROM python:3.13.12-slim@sha256:f1927c75e81efd1e091dbd64b6c0ecaa5630b38635a3d1c04034ac636e1f94c8

WORKDIR /oracle
COPY requirements.txt .
RUN python -m pip install --no-cache-dir --require-hashes -r requirements.txt
COPY generate.py compare_real_fixture.py ./

ENTRYPOINT ["python", "/oracle/generate.py"]
CMD ["/output/py-fsrs-6.3.1-vectors.json"]
