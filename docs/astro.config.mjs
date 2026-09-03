import { defineConfig } from "astro/config";
import starlight from "@astrojs/starlight";

// Published to GitHub Pages at https://pascalinthecloud.github.io/terrastrata,
// so every internal link has to go through `base`. Starlight handles that for
// sidebar and Markdown links; hand-written HTML would not.
export default defineConfig({
  site: "https://pascalinthecloud.github.io",
  base: "/terrastrata",
  integrations: [
    starlight({
      title: "terrastrata",
      description:
        "Pull-through provider cache registry for Terraform and OpenTofu.",
      social: [
        {
          icon: "github",
          label: "GitHub",
          href: "https://github.com/pascalinthecloud/terrastrata",
        },
      ],
      editLink: {
        baseUrl:
          "https://github.com/pascalinthecloud/terrastrata/edit/main/docs/",
      },
      lastUpdated: true,
      sidebar: [
        {
          label: "Start here",
          items: [
            { label: "What terrastrata is", link: "/start/overview/" },
            { label: "Install", link: "/start/install/" },
            { label: "Point your clients at it", link: "/start/clients/" },
          ],
        },
        {
          label: "Guides",
          items: [
            { label: "Mirroring several registries", link: "/guides/multiple-registries/" },
            { label: "Module registry", link: "/guides/module-registry/" },
            { label: "High availability", link: "/guides/high-availability/" },
            { label: "Observability", link: "/guides/observability/" },
            { label: "Supply chain", link: "/guides/supply-chain/" },
            { label: "Troubleshooting", link: "/guides/troubleshooting/" },
          ],
        },
        {
          label: "Reference",
          items: [
            { label: "Configuration", link: "/reference/configuration/" },
            { label: "Helm values", link: "/reference/helm-values/" },
            { label: "Metrics", link: "/reference/metrics/" },
            { label: "Cache layout", link: "/reference/cache-layout/" },
          ],
        },
        { label: "Examples", link: "/examples/" },
      ],
    }),
  ],
});
