import { cookies } from "next/headers";
import { redirect } from "next/navigation";
import { cacheBackend, SESSION_COOKIE, verifySession } from "@/lib/auth";
import { Admin } from "@/components/admin";

export default async function Page() {
  const session = verifySession((await cookies()).get(SESSION_COOKIE)?.value);
  if (!session) redirect("/login");
  return <Admin key={session.nonce} session={session} backend={cacheBackend()} />;
}
