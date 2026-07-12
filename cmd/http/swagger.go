package main

// @title       BookEat backend-core API
// @version     1.0
// @description Core backend service for BookEat.
// @description
// @description Every response is wrapped in a JSON envelope — {"data": ...} on
// @description success, {"error": "..."} on failure — except /.well-known/jwks.json,
// @description which returns a raw JWKS document.

// @securityDefinitions.apikey BearerAuth
// @in                         header
// @name                       Authorization
// @description Access token from POST /api/v1/auth/login (or signup / otp/verify /
// @description refresh), sent as: Authorization: Bearer <access_token>

// The functions below are never called. They exist only to attach swaggo
// annotations to the health and JWKS routes, which are registered as inline
// closures in internal/bootstrap/app.go and therefore have no handler function
// of their own for swag to read.

// swaggerHealth documents GET /health.
// @Summary     Liveness probe
// @Description Returns 200 while the process is up. Does not check dependencies.
// @Tags        system
// @Produce     json
// @Success     200 {object} map[string]interface{} "{\"data\":{\"status\":\"ok\"}}"
// @Router      /health [get]
func swaggerHealth() {}

// swaggerReady documents GET /health/ready.
// @Summary     Readiness probe
// @Description Pings the database. Returns 200 when reachable, 503 otherwise.
// @Tags        system
// @Produce     json
// @Success     200 {object} map[string]interface{} "{\"data\":{\"status\":\"ready\"}}"
// @Failure     503 {object} map[string]interface{} "{\"data\":{\"status\":\"unavailable\"}}"
// @Router      /health/ready [get]
func swaggerReady() {}

// swaggerJWKS documents GET /.well-known/jwks.json.
// @Summary     JWKS
// @Description Public RS256 keys used to verify access-token signatures. This
// @Description response is NOT wrapped in the standard envelope.
// @Tags        system
// @Produce     json
// @Success     200 {object} map[string]interface{} "{\"keys\":[{\"kty\":\"RSA\",\"use\":\"sig\",\"alg\":\"RS256\",\"kid\":\"...\",\"n\":\"...\",\"e\":\"AQAB\"}]}"
// @Router      /.well-known/jwks.json [get]
func swaggerJWKS() {}
