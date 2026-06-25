.PHONY: build deploy synth clean

LAMBDAS = planner worker finalizer orchestrator

build:
	@for fn in $(LAMBDAS); do \
		echo "Building $$fn..."; \
		GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o bootstrap ./cmd/$$fn && \
		zip -j infra/lambda/$$fn.zip bootstrap && \
		rm bootstrap; \
	done

synth: build
	cd infra && npx cdk synth

deploy: build
	cd infra && npx cdk deploy --require-approval never

clean:
	rm -f infra/lambda/*.zip bootstrap
