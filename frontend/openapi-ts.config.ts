import { defineConfig } from '@hey-api/openapi-ts'
export default defineConfig({input:'../api/openapi.yaml',output:'src/api/generated',plugins:['@hey-api/typescript','@hey-api/sdk','@hey-api/client-fetch']})
