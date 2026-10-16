import type { NextConfig } from 'next';
import { PHASE_DEVELOPMENT_SERVER } from 'next/constants';

const config = (phase: string): NextConfig => ({
  output: 'standalone',
  poweredByHeader: false,
  // Keep production builds from overwriting the running dev server's chunks.
  distDir: phase === PHASE_DEVELOPMENT_SERVER ? '.next-dev' : '.next',
});
export default config;
