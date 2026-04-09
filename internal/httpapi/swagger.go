package httpapi

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed swagger.json
var swaggerSpec []byte

func convertRefs(data interface{}) interface{} {
	switch v := data.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})
		for key, value := range v {
			if key == "$ref" {
				if refStr, ok := value.(string); ok {
					if strings.HasPrefix(refStr, "#/definitions/") {
						value = strings.Replace(refStr, "#/definitions/", "#/components/schemas/", 1)
					}
				}
				result[key] = value
			} else {
				result[key] = convertRefs(value)
			}
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(v))
		for i, item := range v {
			result[i] = convertRefs(item)
		}
		return result
	default:
		return v
	}
}

func convertSwagger2ToOpenAPI3(swagger2Spec map[string]interface{}) map[string]interface{} {
	openAPI3 := make(map[string]interface{})
	openAPI3["openapi"] = "3.0.3"

	if info, ok := swagger2Spec["info"].(map[string]interface{}); ok {
		openAPI3["info"] = info
	}

	openAPI3["servers"] = []map[string]interface{}{
		{"url": "http://localhost:4001", "description": "Local development"},
		{"url": "https://marketflowai.codevertexitsolutions.com", "description": "Production"},
	}

	components := make(map[string]interface{})
	if securityDefs, ok := swagger2Spec["securityDefinitions"].(map[string]interface{}); ok {
		securitySchemes := make(map[string]interface{})
		for key, value := range securityDefs {
			securitySchemes[key] = value
		}
		components["securitySchemes"] = securitySchemes
	}
	if definitions, ok := swagger2Spec["definitions"].(map[string]interface{}); ok {
		components["schemas"] = definitions
	}
	if len(components) > 0 {
		openAPI3["components"] = components
	}

	if security, ok := swagger2Spec["security"].([]interface{}); ok {
		openAPI3["security"] = security
	}

	basePath := "/api/v1"
	if bp, ok := swagger2Spec["basePath"].(string); ok && bp != "" {
		basePath = strings.TrimSuffix(bp, "/")
	}

	if paths, ok := swagger2Spec["paths"].(map[string]interface{}); ok {
		convertedPaths := make(map[string]interface{})
		for path, pathItem := range paths {
			if !strings.HasPrefix(path, "/") {
				path = "/" + path
			}
			if basePath != "" && !strings.HasPrefix(path, basePath) {
				path = basePath + path
			}
			convertedPaths[path] = convertRefs(pathItem)
		}
		openAPI3["paths"] = convertedPaths
	}

	if components, ok := openAPI3["components"].(map[string]interface{}); ok {
		if schemas, ok := components["schemas"].(map[string]interface{}); ok {
			components["schemas"] = convertRefs(schemas)
		}
		openAPI3["components"] = components
	}

	return openAPI3
}

// OpenAPIJSON serves the OpenAPI/Swagger JSON specification.
func OpenAPIJSON(w http.ResponseWriter, r *http.Request) {
	if r.Method == "OPTIONS" {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-API-Key")
		w.Header().Set("Access-Control-Max-Age", "3600")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var spec map[string]interface{}
	if err := json.Unmarshal(swaggerSpec, &spec); err == nil {
		if swaggerVersion, ok := spec["swagger"].(string); ok && strings.HasPrefix(swaggerVersion, "2.") {
			openAPI3Spec := convertSwagger2ToOpenAPI3(spec)
			if modifiedSpec, err := json.Marshal(openAPI3Spec); err == nil {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Write(modifiedSpec)
				return
			}
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(swaggerSpec)
}

// SwaggerUI serves the Swagger UI HTML page.
func SwaggerUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!DOCTYPE html>
<html>
  <head>
    <meta charset="UTF-8">
    <title>MarketFlow AI Docs</title>
    <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  </head>
  <body>
    <div id="swagger-ui"></div>
    <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
    <script>
      window.onload = () => {
        const specUrl = window.location.protocol + '//' + window.location.host + '/api/v1/openapi.json';
        window.ui = SwaggerUIBundle({
          url: specUrl,
          dom_id: '#swagger-ui',
          presets: [SwaggerUIBundle.presets.apis, SwaggerUIBundle.SwaggerUIStandalonePreset],
          layout: "BaseLayout",
          deepLinking: true,
          filter: true,
          persistAuthorization: true,
          tryItOutEnabled: true,
          requestInterceptor: (request) => {
            request.credentials = 'omit';
            if (request.headers && request.headers.Authorization) {
              const authHeader = request.headers.Authorization;
              if (!/^bearer\s+/i.test(authHeader)) {
                request.headers.Authorization = 'Bearer ' + authHeader.trim();
              }
            }
            return request;
          }
        })
      }
    </script>
  </body>
</html>`))
}
