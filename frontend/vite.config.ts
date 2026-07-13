import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
export default defineConfig({plugins:[react()],server:{port:43847,proxy:{'/api':{target:'http://127.0.0.1:43846'},'/events':{target:'http://127.0.0.1:43846'},'/mcp':{target:'http://127.0.0.1:43846'}}},build:{outDir:'dist',emptyOutDir:true}})
