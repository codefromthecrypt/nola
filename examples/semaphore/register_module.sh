curl -w "\n" -X POST "http://localhost:9090/api/v1/register-module" -H "namespace: examples" -H "module_id: semaphore" --data-binary "@examples/semaphore/main.wasm"
