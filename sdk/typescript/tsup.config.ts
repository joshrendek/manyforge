import { defineConfig } from 'tsup';

export default defineConfig({
    entry: ['src/index.ts', 'src/public.ts', 'src/server.ts'],
    format: ['esm', 'cjs'],
    target: 'es2022',
    platform: 'neutral',
    dts: true,
    splitting: true,
    sourcemap: true,
    clean: true,
    treeshake: true,
});
