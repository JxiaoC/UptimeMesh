const iconHref = `${import.meta.env.BASE_URL}uptimemesh-icon-64.png`
const fallbackHref = `${import.meta.env.BASE_URL}favicon.ico`
const iconSize = 64
let iconImage: HTMLImageElement | undefined
let iconLoad: Promise<HTMLImageElement> | undefined
let renderVersion = 0

function loadIcon(): Promise<HTMLImageElement> {
  if (iconImage?.complete) return Promise.resolve(iconImage)
  if (iconLoad) return iconLoad

  iconLoad = new Promise((resolve, reject) => {
    const image = new Image()
    image.onload = () => {
      iconImage = image
      resolve(image)
    }
    image.onerror = reject
    image.src = iconHref
  })
  return iconLoad
}

function iconLink(): HTMLLinkElement {
  let link = document.querySelector<HTMLLinkElement>('link[rel~="icon"]')
  if (!link) {
    link = document.createElement('link')
    link.rel = 'icon'
    document.head.append(link)
  }
  return link
}

export async function setFaviconAlertCount(count: number): Promise<void> {
  const version = ++renderVersion
  const link = iconLink()
  if (count <= 0) {
    link.type = 'image/x-icon'
    link.href = fallbackHref
    return
  }

  try {
    const image = await loadIcon()
    if (version !== renderVersion) return
    const canvas = document.createElement('canvas')
    canvas.width = iconSize
    canvas.height = iconSize
    const context = canvas.getContext('2d')
    if (!context) return

    context.drawImage(image, 0, 0, iconSize, iconSize)
    const label = count > 99 ? '99+' : String(count)
    const badgeWidth = iconSize - 4
    const badgeHeight = 40
    const x = (iconSize - badgeWidth) / 2
    const y = iconSize - badgeHeight
    context.beginPath()
    context.roundRect(x, y, badgeWidth, badgeHeight, badgeHeight / 2)
    context.fillStyle = '#e53935'
    context.fill()
    context.fillStyle = '#ffffff'
    context.font = 'bold 32px Arial, sans-serif'
    context.textAlign = 'center'
    context.textBaseline = 'middle'
    context.fillText(label, iconSize / 2, y + badgeHeight / 2 + 0.5)
    link.type = 'image/png'
    link.href = canvas.toDataURL('image/png')
  } catch {
    if (version === renderVersion) {
      link.href = fallbackHref
      link.type = 'image/x-icon'
    }
  }
}
