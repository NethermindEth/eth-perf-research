PROTOC ?= protoc
PROTO_DIR := ../proto
PB_OUT := builder_worker/_proto

.PHONY: proto test

proto:
	mkdir -p $(PB_OUT)
	touch $(PB_OUT)/__init__.py
	# Generate only builder.proto; txsigner.proto is already registered by orchestrator-py.
	# We still pass txsigner.proto as an import so protoc resolves the reference,
	# but we do NOT import the generated txsigner_pb2 from this package.
	$(PROTOC) -I=$(PROTO_DIR) --python_out=$(PB_OUT) --pyi_out=$(PB_OUT) \
		$(PROTO_DIR)/txsigner.proto $(PROTO_DIR)/builder.proto
	# Redirect txsigner_pb2 import to orchestrator._proto so there is only one
	# copy of the descriptor in the pool at runtime.
	sed -i.bak 's/^from \. import txsigner_pb2/from orchestrator._proto import txsigner_pb2/' $(PB_OUT)/builder_pb2.py
	sed -i.bak 's/^import txsigner_pb2/from orchestrator._proto import txsigner_pb2/' $(PB_OUT)/builder_pb2.py
	sed -i.bak 's/^from \. import txsigner_pb2/from orchestrator._proto import txsigner_pb2/' $(PB_OUT)/builder_pb2.pyi 2>/dev/null || true
	sed -i.bak 's/^import txsigner_pb2/from orchestrator._proto import txsigner_pb2/' $(PB_OUT)/builder_pb2.pyi 2>/dev/null || true
	rm -f $(PB_OUT)/*.bak
	# txsigner_pb2 is provided by orchestrator-py; delete the local copy + any cached bytecode.
	rm -f $(PB_OUT)/txsigner_pb2.py $(PB_OUT)/txsigner_pb2.pyi
	rm -f $(PB_OUT)/__pycache__/txsigner_pb2.*.pyc

ORCHESTRATOR_PY ?= $(abspath ../../orchestrator-py)

test:
	uv run \
		--with pytest \
		--with protobuf \
		--with "orchestrator @ file://$(ORCHESTRATOR_PY)" \
		pytest tests/ -v
