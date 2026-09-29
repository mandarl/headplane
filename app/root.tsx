import type { MetaFunction } from "react-router";
import {
  Links,
  Meta,
  Outlet,
  Scripts,
  ScrollRestoration,
  createCookie,
  unstable_useRoute as useRoute,
} from "react-router";

import { LiveDataProvider } from "~/utils/live-data";
import ToastProvider from "~/utils/toast-provider";

import type { Route } from "./+types/root";
import { ErrorBanner } from "./components/error-banner";

import "@fontsource-variable/inter/opsz.css";
import "./tailwind.css";
import { type ColorScheme, isValidColorScheme } from "./utils/color-scheme";

export const meta: MetaFunction = () => [
  { title: "Headplane" },
  {
    name: "description",
    content: "A frontend for the headscale coordination server",
  },
];

// The color-scheme cookie is unsigned (same attributes as the server-side
// `getColorScheme`), so the SPA can read it straight from `document.cookie`.
// `Request.headers` never carries cookies in the browser, hence no `request`
// parameter here.
const colorSchemeCookie = createCookie("color_scheme", {
  maxAge: 34560000,
  sameSite: "lax",
});

export async function clientLoader(): Promise<{ colorScheme: ColorScheme }> {
  const parsed = await colorSchemeCookie.parse(document.cookie || null);
  const colorScheme = (parsed as { colorScheme?: unknown } | null)?.colorScheme;
  return { colorScheme: isValidColorScheme(colorScheme) ? colorScheme : "system" };
}

export function HydrateFallback() {
  return (
    <html lang="en">
      <head>
        <meta charSet="utf-8" />
        <meta content="width=device-width, initial-scale=1" name="viewport" />
      </head>
      <body className="w-full overflow-x-hidden overscroll-none dark:bg-mist-900 dark:text-mist-50">
        <div className="flex h-screen w-screen items-center justify-center">
          <p className="text-sm text-mist-500 dark:text-mist-400">Loading Headplane…</p>
        </div>
      </body>
    </html>
  );
}

export function Layout({ children }: { readonly children: React.ReactNode }) {
  const { loaderData } = useRoute("root");

  // LiveDataProvider is wrapped at the top level since dialogs and things
  // that control its state are usually open in portal containers which
  // are not a part of the normal React tree.
  return (
    <LiveDataProvider>
      <html
        lang="en"
        className={
          loaderData?.colorScheme === "dark"
            ? "dark"
            : loaderData?.colorScheme === "light"
              ? "light"
              : ""
        }
      >
        <head>
          <meta charSet="utf-8" />
          <meta content="width=device-width, initial-scale=1" name="viewport" />
          <Meta />
          <Links />
          <link href={`${__PREFIX__}/favicon.ico`} rel="icon" />
        </head>
        <body className="w-full overflow-x-hidden overscroll-none dark:bg-mist-900 dark:text-mist-50">
          {children}
          <ToastProvider />
          <ScrollRestoration />
          <Scripts />
        </body>
      </html>
    </LiveDataProvider>
  );
}

export function ErrorBoundary({ error }: Route.ErrorBoundaryProps) {
  return (
    <div className="flex h-screen w-screen items-center justify-center p-4">
      <ErrorBanner className="max-w-2xl" error={error} />
    </div>
  );
}

export default function App() {
  return <Outlet />;
}
