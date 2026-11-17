import SwaggerUI from "swagger-ui-dist/swagger-ui-bundle.js";
import "swagger-ui-dist/swagger-ui.css";

SwaggerUI({
 url: "/docs/openapi.json",
 dom_id: "#swagger-ui",
 deepLinking: true,
 validatorUrl: null,
 persistAuthorization: false,
 supportedSubmitMethods: ["get", "post", "put", "patch", "delete"],
});
