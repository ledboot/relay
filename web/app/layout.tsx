import type { Metadata } from 'next';
import './globals.css';
export const metadata: Metadata = {title:'Relay · 短信验证',description:'使用 CD Key 兑换号码，安全接收短信验证码',robots:{index:false,follow:false},icons:{icon:[{url:'/favicon.ico?v=20261015',sizes:'16x16 32x32 48x48'},{url:'/favicon-32x32.png?v=20261015',sizes:'32x32',type:'image/png'},{url:'/icon-192.png?v=20261015',sizes:'192x192',type:'image/png'},{url:'/icon-512.png?v=20261015',sizes:'512x512',type:'image/png'}],apple:[{url:'/apple-touch-icon.png?v=20261015',sizes:'180x180',type:'image/png'}]}};
export default function Layout({children}:{children:React.ReactNode}) {return <html lang="zh-CN"><body>{children}</body></html>}
