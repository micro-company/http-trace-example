# ---- demo.sh -----------------------------------------------------------
# Minimal walkthrough of the CRUD API running on http://localhost:8080
# Each curl shows the full HTTP response (-i) so you can see status codes
# and trigger the error-handling paths for OpenTelemetry.

# 1. Create two items
curl -i -X POST http://localhost:8080/items \
     -H 'Content-Type: application/json' \
     -d '{"name":"widget"}'

curl -i -X POST http://localhost:8080/items \
     -H 'Content-Type: application/json' \
     -d '{"name":"gadget"}'

# 2. List all items
curl -i http://localhost:8080/items

# 3. Fetch the first item
curl -i http://localhost:8080/items/1

# 4. Update the first item
curl -i -X PUT http://localhost:8080/items/1 \
     -H 'Content-Type: application/json' \
     -d '{"name":"widget v2"}'

# 5. Delete the first item
curl -i -X DELETE http://localhost:8080/items/1

# 6. Trigger a “not found” error (recorded as a span error)
curl -i http://localhost:8080/items/1     # should return 404

# 7. Trigger an “invalid id” error (bad path parameter)
curl -i http://localhost:8080/items/abc   # should return 400
# -----------------------------------------------------------------------
