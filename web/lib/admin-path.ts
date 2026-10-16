export function getAdminPath(): string {
  const path = (process.env.ADMIN_PATH || '/console').trim().replace(/\/+$/, '');
  const first = path.split('/')[1]?.toLowerCase();
  if (!/^\/[a-zA-Z0-9_-]+(?:\/[a-zA-Z0-9_-]+)*$/.test(path)
    || ['admin', 'api', '_next', 'branding', 'platforms'].includes(first)) {
    throw new Error('ADMIN_PATH 必须为有效的自定义页面路径，不能使用 /admin、/api 或其他保留入口');
  }
  return path;
}
