import { ImageResponse } from 'next/og'
import path from 'path';
import { readFile } from 'fs/promises';

export const runtime = 'edge'

export async function GET(req: Request): Promise<Response> {
  const fontPath = path.join(process.cwd(), 'assets', 'Inter-SemiBold.otf');
  const fontData = await readFile(fontPath);

  try {
    const { searchParams } = new URL(req.url)

    // ?title=<title>
    const title =
      searchParams.get('title')?.slice(0, 75) || 'eRPC Docs'

    return new ImageResponse(
      (
        <div
          tw="text-white px-20 py-[70px] bg-[#030303] h-full w-full flex justify-between flex-col"
          style={{
            backgroundImage:
              'radial-gradient(circle at 25px 25px, #333 2%, transparent 0%), radial-gradient(circle at 75px 75px, #333 2%, transparent 0%)',
            backgroundSize: '100px 100px',
            backgroundPosition: '-30px -10px'
          }}
        >
          <img height="40" src={`https://erpc-test.up.railway.app/assets/og.jpeg`} />
          <h1
            tw="text-transparent text-[82px] m-0 mb-10 tracking-tighter leading-[1.1]"
            style={{
              textShadow: '0 2px 30px #000',
              backgroundImage: 'linear-gradient(90deg, #fff 40%, #aaa)',
              backgroundClip: 'text'
            }}
          >
            {title}
          </h1>
          <p tw="m-0 text-3xl tracking-tight">
          open-source fault-tolerant evm rpc proxy and cache
          </p>
        </div>
      ),
      {
        width: 1200,
        height: 630,
        fonts: [
          {
            name: 'inter',
            data: fontData,
            style: 'normal'
          }
        ]
      }
    )
  } catch (error) {
    console.error(error)
    return new Response('Failed to generate the image', { status: 500 })
  }
}