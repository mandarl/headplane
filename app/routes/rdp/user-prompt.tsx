import { useEffect, useState } from "react";
import { useNavigate } from "react-router";

import Button from "~/components/button";
import Card from "~/components/card";
import Code from "~/components/code";
import Input from "~/components/input";

const LS_KEY = (hostname: string) => `headplane:rdp_user:${hostname}`;

interface RDPUserPromptProps {
  hostname: string;
}

export default function RDPUserPrompt({ hostname }: RDPUserPromptProps) {
  const navigate = useNavigate();
  const [cachedUser, setCachedUser] = useState("");

  useEffect(() => {
    setCachedUser(localStorage.getItem(LS_KEY(hostname)) ?? "");
  }, [hostname]);

  useEffect(() => {
    import("./wasm.client").then(({ loadHeadplaneRDPWASM }) => {
      loadHeadplaneRDPWASM().catch(() => {});
    });
  }, []);

  function handleSubmit(e: React.FormEvent<HTMLFormElement>) {
    e.preventDefault();
    const formData = new FormData(e.currentTarget);
    const username = formData.get("user")?.toString();
    const password = formData.get("password")?.toString() ?? "";
    const domain = formData.get("domain")?.toString() ?? "";
    const colorDepth = parseInt(formData.get("colorDepth")?.toString() ?? "24", 10);
    if (!username || !password) return;

    localStorage.setItem(LS_KEY(hostname), username);
    navigate(`?user=${encodeURIComponent(username)}`, { state: { password, domain, colorDepth } });
  }

  return (
    <div className="flex h-screen items-center justify-center">
      <Card>
        <Card.Title>Remote Desktop</Card.Title>
        <Card.Text className="mb-4">
          Enter credentials to connect to <Code>{hostname}</Code> via RDP.
        </Card.Text>
        <form onSubmit={handleSubmit}>
          <Input
            labelHidden
            type="text"
            label="Username"
            name="user"
            defaultValue={cachedUser}
            placeholder="Username"
            className="mb-2"
            required
          />
          <Input
            labelHidden
            type="password"
            label="Password"
            name="password"
            placeholder="Password"
            className="mb-2"
            required
          />
          <Input
            labelHidden
            type="text"
            label="Domain (optional)"
            name="domain"
            placeholder="Domain (optional)"
            className="mb-2"
          />
          <div className="mb-4">
            <label className="block text-xs font-medium text-mist-400 mb-1">Color Depth</label>
            <select
              name="colorDepth"
              defaultValue="24"
              className="w-full rounded-md border border-mist-600 bg-mist-900 px-3 py-2 text-sm text-mist-100 focus:outline-none focus:ring-1 focus:ring-mist-400"
            >
              <option value="24">24-bit (recommended)</option>
              <option value="16">16-bit (faster, lower quality)</option>
            </select>
          </div>
          <Button type="submit" variant="heavy" className="w-full">
            Connect
          </Button>
        </form>
      </Card>
    </div>
  );
}
