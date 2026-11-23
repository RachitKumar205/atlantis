// Addresses and copy that index.astro and index.md both read.
export const site = {
  origin: "https://tryatlantis.dev",
  title: "Atlantis",
  headline: "Let AI agents into your production database",
  description:
    "Atlantis sits between your agents and your data. Coding agents ship schema " +
    "changes, production agents read and write rows, and every request runs " +
    "through rules you set.",
  docs: "https://docs.tryatlantis.dev",
  install: "https://docs.tryatlantis.dev/getting-started/",
  platform: "https://platform.tryatlantis.dev",
  enquiries: "https://platform.tryatlantis.dev/api/enquiries",
  // The "Talk to us" URL. Null leaves Sign up as the primary action.
  contact: null as string | null,
  installCommand: "curl -fsSL https://releases.tryatlantis.dev/install.sh | sh",
} as const;

// The text "Copy setup prompt" puts on the clipboard, for a coding agent.
export const agentPrompt = [
  "Set up Atlantis in this repository.",
  `1. Install the tide CLI: ${site.installCommand}`,
  `2. Read ${site.docs}/llms.txt and follow the getting-started guide.`,
  "3. Run `tide init --caller <name>` with the caller name I registered in the console.",
  "4. Run `tide login`. I will approve the login in my browser.",
  "5. Declare the schema in .atl files. Run `tide plan`, then `tide rehearse` if an admin has enabled rehearsal for this caller.",
  "6. File changes with `tide apply`. By default, an admin approves every change that is not additive, in the console.",
].join("\n");
