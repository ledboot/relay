import { notFound } from 'next/navigation';
import Admin from '../../components/admin-page';
import { getAdminPath } from '../../lib/admin-path';

// Read runtime configuration so changing the path does not require an image rebuild.
export const dynamic = 'force-dynamic';

export default async function ManagementPage({ params }: {
  params: Promise<{ managementPath: string[] }>;
}) {
  const { managementPath } = await params;
  if ('/' + managementPath.join('/') !== getAdminPath()) notFound();
  return <Admin />;
}
