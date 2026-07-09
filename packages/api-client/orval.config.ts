import { defineConfig } from 'orval';

export default defineConfig({
  sdano: {
    input: './openapi.json',
    output: {
      target: './src/generated/sdano.ts',
      client: 'fetch',
      mode: 'single',
      clean: true,
      baseUrl: '/',
    },
  },
});
