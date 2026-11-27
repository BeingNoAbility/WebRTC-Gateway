import { defineConfig } from 'vitepress'
import { withMermaid } from 'vitepress-plugin-mermaid'
import { katexPlugin, tableCodeWrapPlugin } from './katex-plugin'

export default withMermaid(
  defineConfig({
    base: '/WebRTC-Gateway/',
    cleanUrls: true,
    lastUpdated: true,

    markdown: {
      config(md) {
        md.use(katexPlugin)
        md.use(tableCodeWrapPlugin)
      },
    },

    // Exclude single-version release notes from VitePress routes to protect release.yml pipeline
    srcExclude: ['**/RELEASE_NOTES.md'],

    head: [
      ['link', { rel: 'icon', type: 'image/svg+xml', href: '/WebRTC-Gateway/logo.svg' }],
      ['meta', { name: 'theme-color', content: '#38bdf8' }],
      ['meta', { property: 'og:type', content: 'website' }],
      ['meta', { property: 'og:title', content: 'WebRTC Gateway - 浏览器零客户端 P2P 内网穿透网关' }],
      ['meta', { property: 'og:description', content: '无需安装客户端或插件，使用标准浏览器经 WebRTC P2P 直连访问内网 Web 服务、NAS 与私有云。' }],
    ],

    locales: {
      root: {
        label: '简体中文',
        lang: 'zh-CN',
        title: 'WebRTC Gateway',
        description: '浏览器零客户端 P2P 内网穿透网关',
        themeConfig: {
          logo: '/logo.svg',
          siteTitle: 'WebRTC Gateway',

          nav: [
            { text: '首页', link: '/' },
            { text: '指南概览', link: '/guide/' },
            {
              text: '典型用例',
              items: [
                { text: 'Agent 典型用例', link: '/guide/agent-use-cases' },
                { text: '本地反代协作模式', link: '/guide/reverse-proxy-recipes' },
                { text: '服务端部署用例', link: '/guide/server-use-cases' },
              ],
            },
            { text: '架构与协议', link: '/guide/architecture' },
            { text: '生产部署', link: '/guide/deployment' },
            { text: '故障排查与 FAQ', link: '/guide/troubleshooting' },
            { text: 'GitHub 仓库', link: 'https://github.com/BeingNoAbility/WebRTC-Gateway' },
          ],

          sidebar: [
            {
              text: '用户指南',
              collapsed: false,
              items: [
                { text: '文档概览', link: '/guide/' },
                { text: 'Agent 典型用例', link: '/guide/agent-use-cases' },
                { text: '本地反代协作模式', link: '/guide/reverse-proxy-recipes' },
                { text: '服务端部署用例', link: '/guide/server-use-cases' },
                { text: '系统架构与核心设计', link: '/guide/architecture' },
                { text: '浏览器兼容性矩阵', link: '/guide/browser-compatibility' },
                { text: '生产部署指南', link: '/guide/deployment' },
                { text: '只读文件与目录适配器', link: '/guide/file-server' },
                { text: '安全机制与密码学边界', link: '/guide/security' },
                { text: '故障排查与常见问题', link: '/guide/troubleshooting' },
              ],
            },
            {
              text: '参考',
              collapsed: false,
              items: [
                { text: 'Agent 与服务端配置', link: '/guide/agent-configuration' },
                { text: 'P2PMux V7 协议规范', link: '/guide/protocol-specification' },
                { text: '架构决策记录 (RFC V7)', link: '/rfcs/v7.0-architecture' },
              ],
            },
            {
              text: '贡献者与治理',
              collapsed: true,
              items: [
                { text: '项目核心原则', link: '/CONSTITUTION' },
                { text: '审查清单', link: '/REVIEW_GUIDELINES' },
                { text: '设计边界与架构权衡', link: '/DESIGN_TRADEOFFS' },
                { text: 'AI 辅助开发指南', link: '/AI_DEVELOPMENT_GUIDE' },
                { text: '文档与文案规范', link: '/STYLE_GUIDE' },
                { text: '正式发版核对清单', link: '/RELEASE_CHECKLIST' },
                { text: '测试策略与验证清单', link: '/TESTING_CHECKLIST' },
              ],
            },
          ],

          docFooter: {
            prev: '上一篇',
            next: '下一篇',
          },

          outline: {
            level: [2, 3],
            label: '本页目录',
          },

          lastUpdated: {
            text: '最后更新于',
          },
        },
      },

      en: {
        label: 'English',
        lang: 'en-US',
        link: '/en/',
        title: 'WebRTC Gateway',
        description: 'Zero-Client P2P WebRTC Intranet Penetration Gateway',
        themeConfig: {
          logo: '/logo.svg',
          siteTitle: 'WebRTC Gateway',

          nav: [
            { text: 'Home', link: '/en/' },
            { text: 'Overview', link: '/en/guide/' },
            {
              text: 'Use Cases',
              items: [
                { text: 'Agent Use Cases', link: '/en/guide/agent-use-cases' },
                { text: 'Local Reverse-Proxy Recipes', link: '/en/guide/reverse-proxy-recipes' },
                { text: 'Server Use Cases', link: '/en/guide/server-use-cases' },
              ],
            },
            { text: 'Architecture', link: '/en/guide/architecture' },
            { text: 'Deployment', link: '/en/guide/deployment' },
            { text: 'FAQ & Troubleshooting', link: '/en/guide/troubleshooting' },
            { text: 'GitHub', link: 'https://github.com/BeingNoAbility/WebRTC-Gateway' },
          ],

          sidebar: [
            {
              text: 'User Guide',
              collapsed: false,
              items: [
                { text: 'Overview & Quickstart', link: '/en/guide/' },
                { text: 'Agent Use Cases', link: '/en/guide/agent-use-cases' },
                { text: 'Local Reverse-Proxy Recipes', link: '/en/guide/reverse-proxy-recipes' },
                { text: 'Server Use Cases', link: '/en/guide/server-use-cases' },
                { text: 'Architecture & Design', link: '/en/guide/architecture' },
                { text: 'Browser Compatibility', link: '/en/guide/browser-compatibility' },
                { text: 'Production Deployment', link: '/en/guide/deployment' },
                { text: 'Read-Only Static Adapter', link: '/en/guide/file-server' },
                { text: 'Security & Cryptography', link: '/en/guide/security' },
                { text: 'Troubleshooting & FAQ', link: '/en/guide/troubleshooting' },
              ],
            },
            {
              text: 'Reference',
              collapsed: false,
              items: [
                { text: 'Agent & Server Configuration', link: '/en/guide/agent-configuration' },
                { text: 'P2PMux V7 Wire Protocol', link: '/en/guide/protocol-specification' },
                { text: 'Architecture Decisions (RFC V7)', link: '/en/rfcs/v7.0-architecture' },
              ],
            },
            {
              text: 'Contributors & Governance',
              collapsed: true,
              items: [
                { text: 'Project Constitution', link: '/en/CONSTITUTION' },
                { text: 'Review Guidelines', link: '/en/REVIEW_GUIDELINES' },
                { text: 'Design Boundaries & Trade-offs', link: '/en/DESIGN_TRADEOFFS' },
                { text: 'AI-Assisted Development Guide', link: '/en/AI_DEVELOPMENT_GUIDE' },
                { text: 'Documentation Style Guide', link: '/en/STYLE_GUIDE' },
                { text: 'Release Checklist', link: '/en/RELEASE_CHECKLIST' },
                { text: 'Testing Strategy & Checklist', link: '/en/TESTING_CHECKLIST' },
              ],
            },
          ],

          docFooter: {
            prev: 'Previous Page',
            next: 'Next Page',
          },

          outline: {
            level: [2, 3],
            label: 'On this page',
          },

          lastUpdated: {
            text: 'Last updated',
          },
        },
      },
    },

    themeConfig: {
      search: {
        provider: 'local',
        options: {
          locales: {
            root: {
              translations: {
                button: {
                  buttonText: '搜索文档',
                  buttonAriaLabel: '搜索文档',
                },
                modal: {
                  noResultsText: '无法找到相关结果',
                  resetButtonTitle: '清除查询条件',
                  footer: {
                    selectText: '选择',
                    navigateText: '切换',
                    closeText: '关闭',
                  },
                },
              },
            },
            en: {
              translations: {
                button: {
                  buttonText: 'Search Docs',
                  buttonAriaLabel: 'Search Docs',
                },
                modal: {
                  noResultsText: 'No results found',
                  resetButtonTitle: 'Clear query',
                  footer: {
                    selectText: 'Select',
                    navigateText: 'Navigate',
                    closeText: 'Close',
                  },
                },
              },
            },
          },
        },
      },

      socialLinks: [
        { icon: 'github', link: 'https://github.com/BeingNoAbility/WebRTC-Gateway' },
      ],

      footer: {
        message: 'Released under the MIT License.',
        copyright: 'Copyright © 2026 Zero-Client P2P WebRTC Gateway Authors',
      },
    },

    mermaid: {},
  })
)
